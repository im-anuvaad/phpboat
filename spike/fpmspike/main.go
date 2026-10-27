// Command fpmspike is the phase 1 spike (docs/plan.md §12): it prepares
// plain-PHP sample apps the way phpboat will, runs one php-fpm master per
// app in its own cgroup as its own user, serves them over a throwaway
// FastCGI client + static file handler, and measures memory and latency.
//
// Throwaway code: nothing here is imported by phpboat.
//
//	fpmspike setup -c spike.json           users, dirs, composer install, persist links, pool configs
//	fpmspike serve -c spike.json -addr :8080
//	fpmspike mem   -c spike.json [-for 20s]
//	fpmspike bench -url http://127.0.0.1:8080/ -host hello.test -c 8 -d 20s
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fpmspike setup|serve|mem|bench ...")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	cfgPath := fs.String("c", "spike.json", "config")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address (serve)")
	dur := fs.Duration("for", 0, "sampling duration (mem)")
	url := fs.String("url", "", "bench URL")
	host := fs.String("host", "", "bench Host header")
	conc := fs.Int("conc", 4, "bench concurrency")
	d := fs.Duration("d", 10*time.Second, "bench duration")
	only := fs.String("only", "", "setup: only this app")
	fs.Parse(os.Args[2:])

	var err error
	switch os.Args[1] {
	case "bench":
		err = bench(*url, *host, *conc, *d)
	default:
		var c *Config
		if c, err = loadConfig(*cfgPath); err != nil {
			break
		}
		switch os.Args[1] {
		case "setup":
			err = setup(c, *only)
		case "serve":
			err = serve(c, *addr)
		case "mem":
			err = mem(c, *dur)
		default:
			err = fmt.Errorf("unknown command %q", os.Args[1])
		}
	}
	if err != nil {
		log.Fatal(err)
	}
}

func sh(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func isMounted(p string) bool {
	b, _ := os.ReadFile("/proc/self/mounts")
	return strings.Contains(string(b), " "+p+" ")
}

// prepareChroot makes the spike root, composer and /dev visible inside the
// noble rootfs at the same paths.
func prepareChroot(c *Config, ch string) error {
	for _, p := range []string{c.Root, "/usr/local/lib/phpboat", "/dev"} {
		in := filepath.Join(ch, p)
		if isMounted(in) {
			continue
		}
		if err := os.MkdirAll(in, 0o755); err != nil {
			return err
		}
		if err := sh("mount", "--bind", p, in); err != nil {
			return err
		}
	}
	if in := filepath.Join(ch, "proc"); !isMounted(in) {
		if err := sh("mount", "-t", "proc", "proc", in); err != nil {
			return err
		}
	}
	return sh("cp", "-L", "/etc/resolv.conf", filepath.Join(ch, "etc/resolv.conf"))
}

func setup(c *Config, only string) error {
	for _, d := range []string{c.Root, filepath.Join(c.Root, "apps"), filepath.Join(c.Root, "downloads")} {
		if err := os.MkdirAll(d, 0o711); err != nil {
			return err
		}
		os.Chmod(d, 0o711) // app users must be able to traverse to their own folders
	}
	for _, ac := range c.Apps {
		if only != "" && ac.Name != only {
			continue
		}
		a, err := c.app(ac)
		if err != nil {
			return err
		}
		if a.RT.Chroot != "" {
			if err := prepareChroot(c, a.RT.Chroot); err != nil {
				return err
			}
		}
		if a.UID == 0 {
			if err := sh("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", a.User()); err != nil {
				return err
			}
			if a, err = c.app(ac); err != nil {
				return err
			}
		}
		log.Printf("[%s] setup: php %s, docroot %q, persist %v, before_start %v", a.Name, a.PHP, a.M.Docroot, a.M.Persist, a.M.BeforeStart)
		if err := os.MkdirAll(a.Dir(), 0o711); err != nil {
			return err
		}
		for _, d := range []string{"data", "tmp", "sessions", "run", "composer"} {
			if err := os.MkdirAll(a.Dir(d), 0o700); err != nil {
				return err
			}
			if err := os.Chown(a.Dir(d), int(a.UID), int(a.GID)); err != nil {
				return err
			}
		}
		os.Chmod(a.Dir(), 0o711)

		// Workspace owned by the app user (stands in for the unpacked tarball).
		ws := filepath.Join(c.Root, "downloads", a.Name)
		os.RemoveAll(ws)
		if err := os.MkdirAll(filepath.Dir(ws), 0o711); err != nil {
			return err
		}
		if err := sh("cp", "-a", a.Src, ws); err != nil {
			return err
		}
		if err := sh("chown", "-R", a.User()+":"+a.User(), ws); err != nil {
			return err
		}
		if fileExists(filepath.Join(ws, "composer.json")) {
			if !fileExists(filepath.Join(ws, "composer.lock")) {
				return fmt.Errorf("[%s] composer.json without composer.lock", a.Name)
			}
			bcg := filepath.Join(cgRoot, "build-"+a.Name)
			os.Remove(bcg)
			if err := ensureCgroup(bcg, 512); err != nil {
				return err
			}
			b := *a
			b.envOverride = map[string]string{"HOME": a.Dir("composer"), "COMPOSER_HOME": a.Dir("composer")}
			out := prefixWriter("[" + a.Name + "] composer: ")
			start := time.Now()
			if err := b.runIn(bcg, ws, out, 10*time.Minute, a.RT.CLI, composerPhar, "install", "--no-dev", "--no-interaction", "--no-progress", "--prefer-dist", "--optimize-autoloader"); err != nil {
				return fmt.Errorf("[%s] composer install: %w", a.Name, err)
			}
			if err := b.runIn(bcg, ws, out, time.Minute, a.RT.CLI, composerPhar, "check-platform-reqs", "--no-dev"); err != nil {
				return fmt.Errorf("[%s] check-platform-reqs: %w", a.Name, err)
			}
			log.Printf("[%s] BUILD composer took %s, build cgroup memory.peak %.1f MB", a.Name, time.Since(start).Round(time.Millisecond), float64(readCgInt(bcg, "memory.peak"))/(1<<20))
			os.Remove(bcg)
		}
		// Immutable build, persist links.
		os.RemoveAll(a.Build())
		if err := os.Rename(ws, a.Build()); err != nil {
			return err
		}
		if err := sh("chown", "-R", "root:root", a.Build()); err != nil {
			return err
		}
		if err := sh("chmod", "-R", "u=rwX,go=rX", a.Build()); err != nil {
			return err
		}
		for _, p := range a.M.Persist {
			in, data := filepath.Join(a.Build(), p), filepath.Join(a.Dir("data"), p)
			if _, err := os.Stat(data); os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(data), 0o700); err != nil {
					return err
				}
				if err := sh("cp", "-a", in, data); err != nil { // first deploy: seed from the repo
					return err
				}
				if err := sh("chown", "-R", a.User()+":"+a.User(), filepath.Join(a.Dir("data"), strings.Split(p, "/")[0])); err != nil {
					return err
				}
			}
			os.RemoveAll(in)
			if err := os.Symlink(data, in); err != nil {
				return err
			}
		}
		if err := os.WriteFile(a.Dir("run", "php-fpm.conf"), []byte(a.poolConf()), 0o644); err != nil {
			return err
		}
		du, _ := exec.Command("du", "-sh", a.Build()).Output()
		log.Printf("[%s] build ready: %s", a.Name, strings.TrimSpace(string(du)))
	}
	return nil
}

func prefixWriter(prefix string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			log.Print(prefix + sc.Text())
		}
	}()
	return pw
}

type runningApp struct {
	*App
	cmd  *exec.Cmd
	site *site
}

func serve(c *Config, addr string) error {
	os.WriteFile(filepath.Join(c.Root, "spike.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	var apps []*runningApp
	sites := map[string]*site{}
	for _, ac := range c.Apps {
		a, err := c.app(ac)
		if err != nil {
			return err
		}
		if a.RT.Chroot != "" {
			if err := prepareChroot(c, a.RT.Chroot); err != nil {
				return err
			}
		}
		// A stale cgroup from an earlier run: kill and recreate.
		if pids, _ := os.ReadFile(filepath.Join(a.Cgroup(), "cgroup.procs")); len(pids) > 0 {
			for _, p := range strings.Fields(string(pids)) {
				n, _ := strconv.Atoi(p)
				syscall.Kill(n, syscall.SIGKILL)
			}
			time.Sleep(200 * time.Millisecond)
		}
		os.Remove(a.Cgroup())
		if err := ensureCgroup(a.Cgroup(), a.MemoryMB); err != nil {
			return err
		}
		os.Remove(a.Sock())
		for _, s := range a.M.BeforeStart {
			rss, d, err := a.runPHP(a.Cgroup(), prefixWriter("["+a.Name+"] before_start: "), 5*time.Minute, s)
			if err != nil {
				return fmt.Errorf("[%s] before_start %s: %w", a.Name, s, err)
			}
			log.Printf("[%s] before_start %s ok in %s, max RSS %.1f MB", a.Name, s, d.Round(time.Millisecond), float64(rss)/1024)
		}
		start := time.Now()
		cmd, err := a.spawn(a.Cgroup(), a.Build(), prefixWriter("["+a.Name+"] fpm: "), a.RT.FPM, "-F", "-O", "-y", a.Dir("run", "php-fpm.conf"))
		if err != nil {
			return fmt.Errorf("[%s] start fpm: %w", a.Name, err)
		}
		br, _ := filepath.EvalSymlinks(a.Build())
		dr, _ := filepath.EvalSymlinks(a.Dir("data"))
		s := &site{app: a, buildReal: br, dataReal: dr, docroot: a.Docroot(), rootMode: a.M.Docroot == "."}
		if err := waitHealthy(s, a.M.HealthPath, 30*time.Second); err != nil {
			return fmt.Errorf("[%s] %w", a.Name, err)
		}
		log.Printf("[%s] READY php %s in %s (pid %d, %s)", a.Name, a.PHP, time.Since(start).Round(time.Millisecond), cmd.Process.Pid, a.Host)
		apps = append(apps, &runningApp{App: a, cmd: cmd, site: s})
		sites[a.Host] = s
	}

	srv := &http.Server{Addr: addr, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := sites[strings.Split(r.Host, ":")[0]]; s != nil {
			s.ServeHTTP(w, r)
			return
		}
		http.Error(w, "unknown host", http.StatusNotFound)
	})}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go cronLoop(ctx, apps)
	go func() {
		<-ctx.Done()
		for _, a := range apps {
			a.cmd.Process.Signal(syscall.SIGQUIT) // FPM: graceful stop
		}
		var wg sync.WaitGroup
		for _, a := range apps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				done := make(chan struct{})
				go func() { a.cmd.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
				}
			}()
		}
		wg.Wait()
		srv.Close()
	}()
	go http.ListenAndServe("127.0.0.1:6060", nil) // pprof (DefaultServeMux)
	log.Printf("serving %d apps on %s", len(apps), addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func waitHealthy(s *site, hp string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.app.Sock()); err == nil {
			req := httptest.NewRequest("GET", "http://"+s.app.Host+hp, nil)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code >= 200 && rec.Code < 400 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("not healthy within %s", max)
}

// cronLoop runs every-minute jobs on the minute (the spike only needs
// "* * * * *"); a job never overlaps itself.
func cronLoop(ctx context.Context, apps []*runningApp) {
	running := map[string]bool{}
	var mu sync.Mutex
	for {
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		for _, a := range apps {
			for i, j := range a.M.Cron {
				if j.Schedule != "* * * * *" {
					continue
				}
				key := fmt.Sprintf("%s/%d", a.Name, i)
				mu.Lock()
				if running[key] {
					mu.Unlock()
					log.Printf("[%s] cron:%d skipped, previous run still going", a.Name, i)
					continue
				}
				running[key] = true
				mu.Unlock()
				go func() {
					rss, d, err := a.runPHP(a.Cgroup(), prefixWriter(fmt.Sprintf("[%s] cron:%d: ", a.Name, i)), time.Hour, j.Run)
					log.Printf("[%s] CRON %s took %s, max RSS %.1f MB, err %v", a.Name, j.Run, d.Round(time.Millisecond), float64(rss)/1024, err)
					mu.Lock()
					delete(running, key)
					mu.Unlock()
				}()
			}
		}
	}
}

// ---- memory ----

type memSample struct {
	current, anon, file, shmem, kernel, pssKB int64
	procs                                     int
}

func cgSample(dir string) memSample {
	var s memSample
	s.current = readCgInt(dir, "memory.current")
	if b, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) != 2 {
				continue
			}
			n, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "anon":
				s.anon = n
			case "file":
				s.file = n
			case "shmem":
				s.shmem = n
			case "kernel":
				s.kernel = n
			}
		}
	}
	pids, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	for _, p := range strings.Fields(string(pids)) {
		s.procs++
		s.pssKB += pss(p)
	}
	return s
}

func pss(pid string) int64 {
	b, err := os.ReadFile("/proc/" + pid + "/smaps_rollup")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Pss:") {
			n, _ := strconv.ParseInt(strings.Fields(l)[1], 10, 64)
			return n
		}
	}
	return 0
}

func meminfo() (total, avail int64) {
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n << 10
		case "MemAvailable:":
			avail = n << 10
		}
	}
	return
}

func mem(c *Config, dur time.Duration) error {
	type agg struct{ last, max memSample }
	res := map[string]*agg{}
	var hostMax, spikeMax int64
	spikePid, _ := os.ReadFile(filepath.Join(c.Root, "spike.pid"))
	end := time.Now().Add(dur)
	for {
		for _, ac := range c.Apps {
			s := cgSample(filepath.Join(cgRoot, ac.Name))
			g := res[ac.Name]
			if g == nil {
				g = &agg{}
				res[ac.Name] = g
			}
			g.last = s
			if s.current > g.max.current {
				g.max.current = s.current
			}
			if s.pssKB > g.max.pssKB {
				g.max.pssKB = s.pssKB
			}
			if s.procs > g.max.procs {
				g.max.procs = s.procs
			}
		}
		t, a := meminfo()
		hostMax = max(hostMax, t-a)
		spikeMax = max(spikeMax, pss(strings.TrimSpace(string(spikePid))))
		if !time.Now().Before(end) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	mb := func(n int64) string { return fmt.Sprintf("%.1f", float64(n)/(1<<20)) }
	fmt.Printf("%-10s %6s %9s %8s %7s %7s %7s %7s %9s %8s\n", "app", "procs", "cur(MB)", "max(MB)", "anon", "file", "shmem", "kernel", "PSS(MB)", "maxPSS")
	var sumCur, sumPSS int64
	for _, ac := range c.Apps {
		g := res[ac.Name]
		sumCur += g.last.current
		sumPSS += g.last.pssKB << 10
		fmt.Printf("%-10s %3d/%-2d %9s %8s %7s %7s %7s %7s %9s %8s\n", ac.Name, g.last.procs, g.max.procs, mb(g.last.current), mb(g.max.current),
			mb(g.last.anon), mb(g.last.file), mb(g.last.shmem), mb(g.last.kernel), mb(g.last.pssKB<<10), mb(g.max.pssKB<<10))
	}
	fmt.Printf("%-10s %6s %9s %8s %7s %7s %7s %7s %9s\n", "apps", "", mb(sumCur), "", "", "", "", "", mb(sumPSS))
	fmt.Printf("spike process max PSS %s MB; host used (total-available) max %s MB\n", mb(spikeMax<<10), mb(hostMax))
	return nil
}

// ---- bench ----

func bench(url, host string, conc int, d time.Duration) error {
	tr := &http.Transport{MaxIdleConnsPerHost: conc, DisableCompression: true}
	cl := &http.Client{Transport: tr, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var mu sync.Mutex
	var lat []time.Duration
	codes := map[int]int{}
	errs := 0
	end := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(end) {
				req, _ := http.NewRequest("GET", url, nil)
				if host != "" {
					req.Host = host
				}
				t := time.Now()
				resp, err := cl.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				el := time.Since(t)
				mu.Lock()
				if err != nil {
					errs++
				} else {
					codes[resp.StatusCode]++
					lat = append(lat, el)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[int(float64(len(lat)-1)*p)]
	}
	fmt.Printf("%s host=%s c=%d: %d req in %s = %.0f req/s, p50 %s p95 %s p99 %s, codes %v, errors %d\n",
		url, host, conc, len(lat), d, float64(len(lat))/d.Seconds(), pct(.5).Round(10*time.Microsecond), pct(.95).Round(10*time.Microsecond), pct(.99).Round(10*time.Microsecond), codes, errs)
	return nil
}
