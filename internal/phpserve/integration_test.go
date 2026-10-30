package phpserve

// Integration tests against a real php-fpm. They run when one is found
// (PHPBOAT_TEST_FPM, or /usr/sbin/php-fpm*) and are skipped otherwise. CI
// installs php-fpm and sets PHPBOAT_REQUIRE_FPM, so there they never skip.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/im-anuvaad/phpboat/internal/fastcgi"
)

func findFPM() string {
	if p := os.Getenv("PHPBOAT_TEST_FPM"); p != "" {
		return p
	}
	m, _ := filepath.Glob("/usr/sbin/php-fpm*")
	if len(m) > 0 {
		return m[len(m)-1]
	}
	return ""
}

type realFPM struct {
	sock string
	logs *lineLog
}

type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) add(s string) { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *lineLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// startFPM runs php-fpm -F with a pool like the one phpboat writes
// (docs/architecture.md §8), as the current user.
func startFPM(t testing.TB) *realFPM {
	t.Helper()
	bin := findFPM()
	if bin == "" {
		if os.Getenv("PHPBOAT_REQUIRE_FPM") != "" {
			t.Fatal("PHPBOAT_REQUIRE_FPM is set but no php-fpm was found")
		}
		t.Skip("no php-fpm found (set PHPBOAT_TEST_FPM)")
	}
	// Not t.TempDir(): its parent is 0700, and as root the workers run as
	// nobody and must reach the temp dir (PHP spools big bodies there).
	dir, err := os.MkdirTemp("", "phpboat-fpm-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	os.Chmod(dir, 0o755)
	sock := filepath.Join(dir, "php.sock")
	tmp := filepath.Join(dir, "tmp")
	os.Mkdir(tmp, 0o777)
	user := ""
	if os.Geteuid() == 0 {
		user = "user = nobody\ngroup = nogroup\nlisten.owner = root\nlisten.mode = 0600\n"
		os.Chmod(tmp, 0o1777)
	}
	conf := fmt.Sprintf(`[global]
error_log = /proc/self/fd/2
systemd_interval = 0
[app]
%slisten = %s
pm = static
pm.max_children = 4
clear_env = no
catch_workers_output = yes
decorate_workers_output = no
php_admin_value[display_errors] = 0
php_admin_value[log_errors] = 1
php_admin_value[output_buffering] = 4096
php_admin_value[upload_max_filesize] = 20M
php_admin_value[post_max_size] = 20M
php_admin_value[upload_tmp_dir] = %s
php_admin_value[sys_temp_dir] = %s
php_admin_value[opcache.enable] = 0
`, user, sock, tmp, tmp)
	cf := filepath.Join(dir, "php-fpm.conf")
	if err := os.WriteFile(cf, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-F", "-O", "-n", "-y", cf}
	// -n skips the distro php.ini but also its extension loading; the
	// tests only need core PHP. Keep conf.d off for reproducibility.
	if os.Geteuid() == 0 {
		args = append(args, "-R")
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "PHPBOAT_TEST_ENV=from-the-dashboard"}
	logs := &lineLog{}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			logs.add("fpm: " + sc.Text())
		}
	}()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGQUIT)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		pw.Close()
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			return &realFPM{sock: sock, logs: logs}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("php-fpm did not start: %v", logs.lines)
	return nil
}

func realSite(t testing.TB, fpm *realFPM) (*Site, *httptest.Server) {
	t.Helper()
	build, _ := filepath.Abs("testdata/app")
	s, err := New(Config{
		Build: build, Docroot: "public",
		FastCGI:  &fastcgi.Client{Address: fpm.sock},
		ClientIP: func(*http.Request) string { return "198.51.100.4" },
		PHPLog:   func(r *http.Request, line string) { fpm.logs.add(r.URL.Path + ": " + line) },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func get(t *testing.T, srv *httptest.Server, path string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Host = "blog.example.com"
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestRealFPM(t *testing.T) {
	fpm := startFPM(t)
	_, srv := realSite(t, fpm)

	t.Run("what PHP sees", func(t *testing.T) {
		resp, body := get(t, srv, "/index.php/extra?x=1&y=two", "X-Forwarded-For", "6.6.6.6", "Proxy", "http://evil")
		if resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		var got struct {
			Server        map[string]any `json:"server"`
			Get           map[string]any `json:"get"`
			Env           any            `json:"env"`
			HTTPSDetected bool           `json:"https_detected"`
			PHP           string         `json:"php"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		build, _ := filepath.Abs("testdata/app/public")
		want := map[string]any{
			"REQUEST_URI": "/index.php/extra?x=1&y=two", "SCRIPT_NAME": "/index.php", "PATH_INFO": "/extra",
			"PHP_SELF": "/index.php/extra", "SCRIPT_FILENAME": build + "/index.php", "DOCUMENT_ROOT": build,
			"REMOTE_ADDR": "198.51.100.4", "HTTPS": "on", "SERVER_PORT": "443", "HTTP_HOST": "blog.example.com",
			"HTTP_X_FORWARDED_FOR": nil, "HTTP_PROXY": nil,
		}
		for k, v := range want {
			if got.Server[k] != v {
				t.Errorf("$_SERVER[%s] = %v, want %v", k, got.Server[k], v)
			}
		}
		if got.Get["y"] != "two" || got.Env != "from-the-dashboard" || !got.HTTPSDetected {
			t.Errorf("get %v env %v https %v", got.Get, got.Env, got.HTTPSDetected)
		}
		t.Logf("PHP %s", got.PHP)
	})

	t.Run("front controller and folders", func(t *testing.T) {
		if _, body := get(t, srv, "/some/pretty/url"); !strings.Contains(body, `"SCRIPT_NAME":"\/index.php"`) {
			t.Errorf("%s", body)
		}
		if _, body := get(t, srv, "/sub/"); body != "sub index /sub/index.php" {
			t.Errorf("%q", body)
		}
		if resp, body := get(t, srv, "/style.css"); resp.Header.Get("Content-Type") != "text/css; charset=utf-8" || !strings.Contains(body, "red") {
			t.Errorf("%v %q", resp.Header, body)
		}
	})

	t.Run("status, cookies, headers", func(t *testing.T) {
		resp, body := get(t, srv, "/headers.php")
		if resp.StatusCode != 201 || body != "created" || len(resp.Cookies()) != 2 || resp.Header.Get("X-Custom") != "yes" {
			t.Errorf("%d %q %v", resp.StatusCode, body, resp.Header)
		}
		resp, _ = get(t, srv, "/redirect.php")
		if resp.StatusCode != 302 || resp.Header.Get("Location") != "/elsewhere" {
			t.Errorf("redirect: %d %v", resp.StatusCode, resp.Header)
		}
	})

	t.Run("big response", func(t *testing.T) {
		_, body := get(t, srv, "/big.php?mb=8")
		if len(body) != 8<<20 || body[0] != 'a' || body[65536] != 'b' || body[len(body)-1] != byte(97+(8*16-1)%26) {
			t.Errorf("got %d bytes", len(body))
		}
	})

	t.Run("streaming is flushed", func(t *testing.T) {
		req, _ := http.NewRequest("GET", srv.URL+"/stream.php", nil)
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		line, _ := br.ReadString('\n')
		first := time.Since(start)
		rest, _ := io.ReadAll(br)
		total := time.Since(start)
		if line != "data: one\n" || !strings.Contains(string(rest), "data: two") {
			t.Errorf("%q %q", line, rest)
		}
		if first > 300*time.Millisecond || total < 350*time.Millisecond {
			t.Errorf("first event after %s, all after %s: not streamed", first, total)
		}
	})

	t.Run("POST form, raw body, chunked body", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/index.php", "application/x-www-form-urlencoded", strings.NewReader("a=1&b=%C3%A4"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), `"post":{"a":"1","b":"\u00e4"}`) {
			t.Errorf("%s", b)
		}
		payload := bytes.Repeat([]byte("0123456789"), 300000) // 3 MB
		sum := md5.Sum(payload)
		for _, chunked := range []bool{false, true} {
			var body io.Reader = bytes.NewReader(payload)
			if chunked {
				body = io.MultiReader(bytes.NewReader(payload[:1000]), bytes.NewReader(payload[1000:])) // unknown length
			}
			req, _ := http.NewRequest("POST", srv.URL+"/raw.php", body)
			if chunked {
				req.ContentLength = -1
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if want := fmt.Sprintf("%d %s", len(payload), hex.EncodeToString(sum[:])); string(b) != want {
				t.Errorf("chunked=%v: %q, want %q", chunked, b, want)
			}
		}
	})

	t.Cleanup(func() {
		if t.Failed() {
			fpm.logs.mu.Lock()
			t.Logf("php-fpm log:\n%s", strings.Join(fpm.logs.lines, "\n"))
			fpm.logs.mu.Unlock()
		}
	})

	t.Run("big body ignored by a script that answers a lot", func(t *testing.T) {
		// FPM writes the response while phpboat is still sending the body:
		// both must make progress (no deadlock until a timeout).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/ignore.php", bytes.NewReader(bytes.Repeat([]byte("z"), 8<<20)))
		req.Header.Set("Content-Type", "application/octet-stream")
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("after %s: %v", time.Since(start), err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(b) != 2<<20 {
			t.Errorf("got %d bytes after %s", len(b), time.Since(start))
		}
	})

	t.Run("multipart upload", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("field", "value")
		fw, _ := mw.CreateFormFile("f", "photo.jpg")
		data := bytes.Repeat([]byte{0xff, 0xd8, 0x00, 0x01}, 1<<20) // 4 MB
		fw.Write(data)
		mw.Close()
		resp, err := http.Post(srv.URL+"/upload.php", mw.FormDataContentType(), &buf)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		sum := md5.Sum(data)
		want := fmt.Sprintf(`{"error":0,"size":%d,"md5":"%s","field":"value"}`, len(data), hex.EncodeToString(sum[:]))
		if string(b) != want {
			t.Errorf("%s\nwant %s", b, want)
		}
	})

	t.Run("PHP log lines and fatal errors", func(t *testing.T) {
		if _, body := get(t, srv, "/log.php"); body != "logged" {
			t.Errorf("%q", body)
		}
		resp, _ := get(t, srv, "/fatal.php")
		if resp.StatusCode != 500 {
			t.Errorf("fatal: %d", resp.StatusCode)
		}
		time.Sleep(100 * time.Millisecond)
		for _, want := range []string{"/log.php: PHP message: hello from error_log", "a user warning", "/fatal.php: PHP message: PHP Fatal error:  Uncaught Error: Call to undefined function this_function_does_not_exist()"} {
			if !fpm.logs.has(want) {
				t.Errorf("log lacks %q:\n%s", want, strings.Join(fpm.logs.lines, "\n"))
			}
		}
	})

	t.Run("concurrency beyond the pool size", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan string, 64)
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := http.Get(srv.URL + "/slow.php?ms=100")
				if err != nil {
					errs <- err.Error()
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || string(b) != "slow done" {
					errs <- fmt.Sprintf("%d %q", resp.StatusCode, b)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
	})

	t.Run("visitor gone mid-request", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/slow.php?ms=1500", nil)
		if _, err := http.DefaultClient.Do(req); err == nil {
			t.Fatal("expected a timeout")
		}
		// The pool is still healthy afterwards.
		if _, body := get(t, srv, "/slow.php?ms=1"); body != "slow done" {
			t.Errorf("%q", body)
		}
	})
}

func BenchmarkRealFPM(b *testing.B) {
	fpm := startFPM(b)
	s, _ := realSite(b, fpm)
	for _, path := range []string{"/sub/", "/style.css"} {
		b.Run(path, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					r := httptest.NewRequest("GET", path, nil)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if w.Code != 200 {
						b.Fatalf("%d", w.Code)
					}
				}
			})
		})
	}
}
