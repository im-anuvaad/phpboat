package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/fastcgi"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/php"
	"github.com/im-anuvaad/phpboat/internal/phpcli"
	"github.com/im-anuvaad/phpboat/internal/phpserve"
	"github.com/im-anuvaad/phpboat/internal/users"
)

// beforeStartTimeout bounds one before_start script (docs/plan.md §5).
const beforeStartTimeout = 5 * time.Minute

// State of an app.
type State string

// States.
const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Backoff  State = "backoff" // waiting to restart after a failure
	Crashed  State = "crashed" // too many failures; needs a manual start
)

// Policy holds the timing rules (docs/architecture.md).
type Policy struct {
	HealthTimeout  time.Duration // max time from spawn to healthy
	StopGrace      time.Duration // SIGQUIT (FPM's graceful stop) → SIGKILL
	BackoffMin     time.Duration
	BackoffMax     time.Duration
	CrashWindow    time.Duration
	CrashLimit     int           // exits within CrashWindow that mark the app crashed
	StableAfter    time.Duration // healthy this long resets the backoff
	OOMPoll        time.Duration
	NoFile         uint64
	HealthInterval time.Duration
}

// DefaultPolicy matches the documented behaviour.
var DefaultPolicy = Policy{
	HealthTimeout:  30 * time.Second,
	StopGrace:      10 * time.Second,
	BackoffMin:     time.Second,
	BackoffMax:     60 * time.Second,
	CrashWindow:    5 * time.Minute,
	CrashLimit:     5,
	StableAfter:    60 * time.Second,
	OOMPoll:        10 * time.Second,
	NoFile:         app.DefaultNoFile,
	HealthInterval: 200 * time.Millisecond,
}

// Status is a snapshot of an app's runtime state.
type Status struct {
	State      State
	PID        int
	Build      string    // the build the process runs (or last ran)
	StartedAt  time.Time // current process spawn time
	HealthyAt  time.Time
	Restarts   int // automatic restarts since the last manual start
	LastExit   string
	LastExitAt time.Time
	LastError  string
	OOMKills   int64
	NextStart  time.Time // when in Backoff
}

// ErrStopped is returned to Start waiters when the app is stopped meanwhile.
var ErrStopped = errors.New("app was stopped")

// ErrNotDeployed is returned by Start for an app that was never deployed.
var ErrNotDeployed = errors.New("app has never been deployed; deploy it first")

type op int

const (
	opStart op = iota
	opStop
	opUpdate
)

type command struct {
	op    op
	spec  app.Spec
	reply chan error
}

type exitEvent struct {
	gen   int
	state *os.ProcessState
	err   error
}

type healthEvent struct {
	gen int
	err error
}

// App is one supervised app: a php-fpm master for its current build,
// and the phpserve handler in front of it. All mutable process state is
// owned by the loop goroutine; Status() reads a mutex-protected snapshot.
type App struct {
	dirs   layout.Layout
	user   users.User
	group  *cgroup.Group
	log    *logs.Log
	policy Policy
	slog   *slog.Logger
	web    Web

	site atomic.Pointer[phpserve.Site] // handler of the current process

	cmds     chan command
	exitCh   chan exitEvent
	healthCh chan healthEvent
	quit     chan struct{}
	quitOnce sync.Once

	mu     sync.RWMutex
	spec   app.Spec
	status Status

	startMu sync.Mutex // one Start at a time, including its before_start scripts

	// loop-owned
	gen          int
	cmd          *exec.Cmd
	desired      State // Running or Stopped
	startWaiters []chan error
	stopWaiters  []chan error
	pendingErr   error // why a starting process is being killed
	exits        []time.Time
	consecutive  int
	lastOOM      int64
	backoff      *time.Timer
	stopTimer    *time.Timer
	healthCancel context.CancelFunc
}

func newApp(spec app.Spec, dirs layout.Layout, u users.User, g *cgroup.Group, lg *logs.Log, p Policy, l *slog.Logger, w Web) *App {
	a := &App{
		dirs: dirs, user: u, group: g, log: lg, policy: p, slog: l, web: w,
		cmds:     make(chan command),
		exitCh:   make(chan exitEvent, 1),
		healthCh: make(chan healthEvent, 1),
		quit:     make(chan struct{}),
		spec:     spec,
		status:   Status{State: Stopped, Build: spec.BuildID},
		desired:  Stopped,
	}
	if st, err := g.Stats(); err == nil {
		a.lastOOM = st.OOMKills
	}
	return a
}

// Spec returns the app's current spec.
func (a *App) Spec() app.Spec {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.spec
}

// Status returns a snapshot of the app's state.
func (a *App) Status() Status {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status
}

// Handler serves the app's requests (static files + PHP), or nil when it
// has never been started. The router only uses it while the app is running.
func (a *App) Handler() http.Handler {
	if s := a.site.Load(); s != nil {
		return s
	}
	return nil
}

// Log returns the app's log (for live tailing).
func (a *App) Log() *logs.Log { return a.log }

// Running reports whether the app is up and healthy.
func (a *App) Running() bool { return a.Status().State == Running }

// RunCron runs `php <script>` for one cron job (docs/app-contract.md §8): as
// the app user, in the app's cgroup, with the app's environment and PHP
// settings, from the build directory. Its output goes to the app log, each
// line with prefix in front. When ctx ends, only the job's own process group
// is killed: the app's FPM workers share the cgroup.
func (a *App) RunCron(ctx context.Context, prefix, script string) (phpcli.Result, error) {
	if a.web.PHP == nil {
		return phpcli.Result{}, php.ErrNotInstalled
	}
	rt, err := a.web.PHP()
	if err != nil {
		return phpcli.Result{}, err
	}
	spec := a.Spec()
	return phpcli.Run(ctx, phpcli.Command{
		Binary: rt.CLI, Ini: a.pool(spec).INI(), Args: []string{script}, Dir: spec.BuildDir,
		Env: a.environ(spec), User: a.user, Group: a.group, Log: a.log,
		Prefix: prefix, SharedGroup: true,
	})
}

// Group returns the app's cgroup (for metrics).
func (a *App) Group() *cgroup.Group { return a.group }

// Start starts the app and waits until it is healthy or has failed. Before
// a stopped app starts, its before_start scripts run in order; if one fails
// the app does not start.
func (a *App) Start(ctx context.Context) error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	// No process runs in these states, so the scripts can start in an empty cgroup.
	if st := a.state(); st == Stopped || st == Crashed || st == Backoff {
		if err := a.runBeforeStart(ctx, a.Spec()); err != nil {
			return err
		}
	}
	return a.do(ctx, command{op: opStart})
}

// Stop stops the app (SIGQUIT: FPM finishes running requests; after the
// grace period the whole cgroup is killed).
func (a *App) Stop(ctx context.Context) error { return a.do(ctx, command{op: opStop}) }

// Restart stops then starts the app.
func (a *App) Restart(ctx context.Context) error {
	if err := a.Stop(ctx); err != nil {
		return err
	}
	return a.Start(ctx)
}

// Update replaces the spec (e.g. a new build). The app must be stopped or
// crashed. Name and user cannot change.
func (a *App) Update(ctx context.Context, spec app.Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	return a.do(ctx, command{op: opUpdate, spec: spec})
}

func (a *App) do(ctx context.Context, c command) error {
	c.reply = make(chan error, 1)
	select {
	case a.cmds <- c:
	case <-a.quit:
		return errors.New("supervisor: app actor is shut down")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) close() {
	a.quitOnce.Do(func() { close(a.quit) })
	a.log.Close()
}

func (a *App) setStatus(f func(*Status)) {
	a.mu.Lock()
	f(&a.status)
	a.mu.Unlock()
}

func (a *App) state() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.status.State
}

func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func (a *App) loop() {
	oom := time.NewTicker(a.policy.OOMPoll)
	defer oom.Stop()
	for {
		select {
		case <-a.quit:
			return
		case c := <-a.cmds:
			switch c.op {
			case opStart:
				a.handleStart(c.reply)
			case opStop:
				a.handleStop(c.reply)
			case opUpdate:
				a.handleUpdate(c)
			}
		case ev := <-a.exitCh:
			a.handleExit(ev)
		case ev := <-a.healthCh:
			a.handleHealth(ev)
		case <-timerC(a.backoff):
			a.backoff = nil
			a.spawn()
		case <-timerC(a.stopTimer):
			a.stopTimer = nil
			a.log.Writef("did not exit within %s after SIGQUIT; killing", a.policy.StopGrace)
			a.killGroup()
		case <-oom.C:
			a.checkOOM()
		}
	}
}

func (a *App) handleStart(reply chan error) {
	if a.Spec().BuildID == "" {
		reply <- ErrNotDeployed
		return
	}
	a.desired = Running
	switch a.state() {
	case Running:
		reply <- nil
	case Starting:
		a.startWaiters = append(a.startWaiters, reply)
	case Stopping:
		reply <- errors.New("app is stopping; try again when it has stopped")
	default: // Stopped, Crashed, Backoff: a manual start resets the crash history
		if a.backoff != nil {
			a.backoff.Stop()
			a.backoff = nil
		}
		a.exits, a.consecutive = nil, 0
		a.setStatus(func(s *Status) { s.Restarts, s.LastError, s.NextStart = 0, "", time.Time{} })
		a.startWaiters = append(a.startWaiters, reply)
		a.spawn()
	}
}

func (a *App) handleStop(reply chan error) {
	a.desired = Stopped
	switch a.state() {
	case Stopped, Crashed, Backoff:
		if a.backoff != nil {
			a.backoff.Stop()
			a.backoff = nil
		}
		a.setStatus(func(s *Status) { s.State, s.NextStart = Stopped, time.Time{} })
		reply <- nil
	case Stopping:
		a.stopWaiters = append(a.stopWaiters, reply)
	case Starting, Running:
		a.stopWaiters = append(a.stopWaiters, reply)
		a.replyStart(ErrStopped)
		a.setStatus(func(s *Status) { s.State = Stopping })
		a.log.Writef("stopping (SIGQUIT to php-fpm pid %d)", a.cmd.Process.Pid)
		if err := a.cmd.Process.Signal(syscall.SIGQUIT); err != nil && !errors.Is(err, os.ErrProcessDone) {
			a.slog.Warn("SIGQUIT failed", "err", err)
		}
		a.stopTimer = time.NewTimer(a.policy.StopGrace)
	}
}

func (a *App) handleUpdate(c command) {
	st := a.state()
	if st != Stopped && st != Crashed {
		c.reply <- fmt.Errorf("app must be stopped to update it (state: %s)", st)
		return
	}
	cur := a.Spec()
	if c.spec.Name != cur.Name {
		c.reply <- errors.New("app name cannot change")
		return
	}
	if err := applyLimits(a.group, c.spec.Limits); err != nil {
		c.reply <- err
		return
	}
	a.mu.Lock()
	a.spec = c.spec
	a.status.Build = c.spec.BuildID
	a.mu.Unlock()
	c.reply <- nil
}

// spawn starts a new process and a health check for it.
func (a *App) spawn() {
	a.gen++
	a.pendingErr = nil
	spec := a.Spec()
	cmd, site, err := a.startProcess(spec)
	if err != nil {
		a.log.Writef("failed to start: %v", err)
		a.setStatus(func(s *Status) { s.LastError = err.Error() })
		a.failure(err)
		return
	}
	a.cmd = cmd
	now := time.Now()
	a.setStatus(func(s *Status) {
		s.State, s.PID, s.StartedAt, s.HealthyAt, s.NextStart = Starting, cmd.Process.Pid, now, time.Time{}, time.Time{}
		s.Build = spec.BuildID
	})
	a.log.Writef("started php-fpm %s, pid %d (build %s, %d workers)", cmd.Path, cmd.Process.Pid, spec.BuildID, spec.Concurrency)

	gen := a.gen
	go func() {
		err := cmd.Wait()
		ev := exitEvent{gen: gen, state: cmd.ProcessState, err: err}
		select {
		case a.exitCh <- ev:
		case <-a.quit:
		}
	}()
	hctx, hcancel := context.WithCancel(context.Background())
	a.healthCancel = hcancel
	go func() {
		defer hcancel()
		go func() {
			select {
			case <-a.quit:
				hcancel()
			case <-hctx.Done():
			}
		}()
		err := healthCheck(hctx, site, a.dirs.Socket(spec.Name), spec, a.policy)
		select {
		case a.healthCh <- healthEvent{gen: gen, err: err}:
		case <-a.quit:
		}
	}()
}

// pool is the FPM pool of spec, which is also the source of the ini
// settings the before_start scripts run with.
func (a *App) pool(spec app.Spec) php.Pool {
	return php.Pool{
		App: spec.Name, Build: spec.BuildDir, DataDir: a.dataDir(spec), TmpDir: a.tmpDir(spec),
		SessionsDir: a.dirs.SessionsDir(spec.Name), Socket: a.dirs.Socket(spec.Name),
		Concurrency: spec.Concurrency, MemoryMax: spec.Limits.MemoryMax, Settings: spec.PHP,
	}
}

// runBeforeStart runs the build's before_start scripts in order, as the app
// user, inside the app's cgroup, with the app's environment and PHP
// settings (docs/app-contract.md §8). Their output goes to the app log.
func (a *App) runBeforeStart(ctx context.Context, spec app.Spec) error {
	if len(spec.BeforeStart) == 0 {
		return nil
	}
	if a.web.PHP == nil {
		return php.ErrNotInstalled
	}
	rt, err := a.web.PHP()
	if err != nil {
		return err
	}
	ini := a.pool(spec).INI()
	env := a.environ(spec)
	for _, script := range spec.BeforeStart {
		a.log.Writef("before_start: php %s", script)
		sctx, cancel := context.WithTimeout(ctx, beforeStartTimeout)
		res, err := phpcli.Run(sctx, phpcli.Command{
			Binary: rt.CLI, Ini: ini, Args: []string{script}, Dir: spec.BuildDir, Env: env,
			User: a.user, Group: a.group, Log: a.log,
		})
		cancel()
		switch {
		case err != nil:
			return fmt.Errorf("before_start %s: %w", script, err)
		case res.TimedOut:
			return fmt.Errorf("before_start %s did not finish within %s", script, beforeStartTimeout)
		case res.ExitCode != 0:
			msg := phpcli.LastLine(res.Tail)
			if msg == "" {
				msg = "no output"
			}
			return fmt.Errorf("before_start %s failed (exit code %d): %s", script, res.ExitCode, msg)
		}
		a.log.Writef("before_start: %s finished", script)
	}
	return nil
}

// startProcess writes the pool config and starts php-fpm in the app's
// cgroup as the app user. It returns the handler for the build.
func (a *App) startProcess(spec app.Spec) (*exec.Cmd, *phpserve.Site, error) {
	if a.web.PHP == nil {
		return nil, nil, php.ErrNotInstalled
	}
	rt, err := a.web.PHP()
	if err != nil {
		return nil, nil, err
	}
	sock := a.dirs.Socket(spec.Name)
	site, err := phpserve.New(phpserve.Config{
		Build: spec.BuildDir, Docroot: spec.Docroot, DataDir: a.dataDir(spec),
		FastCGI:  &fastcgi.Client{Address: sock},
		ClientIP: a.web.ClientIP,
		Error:    a.web.ErrorPage,
		SpoolDir: a.tmpDir(spec),
		PHPLog: func(r *http.Request, line string) {
			a.log.Write(logs.Stderr, r.Method+" "+r.URL.Path+": "+line)
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build %s: %w", spec.BuildID, err)
	}
	conf, err := a.pool(spec).Config()
	if err != nil {
		return nil, nil, err
	}
	if err := writeFileAtomic(a.dirs.FPMConfig(spec.Name), []byte(conf), 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("remove stale socket: %w", err)
	}

	fd, err := a.group.OpenFD()
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(fd)

	// The pipes belong to the app user: FPM reopens /proc/self/fd/2 for its
	// error_log, which fails with EACCES on a root-owned pipe (phase 1, F2).
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, nil, err
	}
	closeAll := func() { outR.Close(); outW.Close(); errR.Close(); errW.Close() }
	for _, w := range []*os.File{outW, errW} {
		if err := w.Chown(int(a.user.UID), int(a.user.GID)); err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("log pipe: %w", err)
		}
	}

	cmd := exec.Command(rt.FPM, "-F", "-O", "-y", a.dirs.FPMConfig(spec.Name))
	cmd.Dir = spec.BuildDir
	cmd.Env = a.environ(spec)
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: a.user.UID, Gid: a.user.GID, Groups: []uint32{}},
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true, // clone3(CLONE_INTO_CGROUP): the child starts inside its cgroup
		CgroupFD:    fd,
	}
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, nil, err
	}

	lim := &unix.Rlimit{Cur: a.policy.NoFile, Max: a.policy.NoFile}
	if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_NOFILE, lim, nil); err != nil {
		a.slog.Warn("setting open-files limit failed", "err", err)
	}

	go func() { a.log.Capture(logs.Stdout, outR); outR.Close() }()
	go func() { a.log.Capture(logs.Stderr, errR); errR.Close() }()
	a.site.Store(site)
	return cmd, site, nil
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// environ builds the contract environment (docs/app-contract.md §7). The
// process does not inherit phpboat's environment.
func (a *App) environ(spec app.Spec) []string { return Environ(a.dirs, spec) }

// Environ is the contract environment of spec's processes.
func Environ(l layout.Layout, spec app.Spec) []string {
	env := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin:/bin",
		"LANG": "C.UTF-8",
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	contract := map[string]string{
		"DATA_DIR":         l.DataDir(spec.Name),
		"TMPDIR":           l.TmpDir(spec.Name),
		"PHPBOAT_APP":      spec.Name,
		"PHPBOAT_DOMAIN":   spec.Domain,
		"PHPBOAT_RELEASE":  spec.BuildID,
		"PHPBOAT_COMMIT":   spec.CommitSHA,
		"PHPBOAT_CONTRACT": strconv.Itoa(app.ContractVersion),
	}
	for k, v := range contract {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (a *App) dataDir(spec app.Spec) string { return a.dirs.DataDir(spec.Name) }
func (a *App) tmpDir(spec app.Spec) string  { return a.dirs.TmpDir(spec.Name) }

func (a *App) handleHealth(ev healthEvent) {
	if ev.gen != a.gen || a.state() != Starting {
		return
	}
	if ev.err == nil {
		now := time.Now()
		a.setStatus(func(s *Status) { s.State, s.HealthyAt = Running, now })
		a.log.Writef("healthy (%s after start)", now.Sub(a.Status().StartedAt).Round(time.Millisecond))
		a.replyStart(nil)
		return
	}
	a.pendingErr = fmt.Errorf("health check failed: %w", ev.err)
	a.log.Writef("%v; killing", a.pendingErr)
	a.killGroup() // the exit event drives the failure policy
}

func (a *App) handleExit(ev exitEvent) {
	if ev.gen != a.gen {
		return
	}
	if a.healthCancel != nil {
		a.healthCancel()
		a.healthCancel = nil
	}
	desc := describeExit(ev)
	// The main process is gone; nothing else of this app may keep running.
	a.killGroup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := a.group.WaitEmpty(ctx); err != nil {
		a.slog.Error("app cgroup not empty after kill", "err", err)
	}
	cancel()
	oom := a.checkOOM()
	if oom {
		desc += " (out of memory)"
	}
	a.cmd = nil
	now := time.Now()
	a.setStatus(func(s *Status) { s.PID, s.LastExit, s.LastExitAt = 0, desc, now })
	a.log.Writef("exited: %s", desc)

	switch a.state() {
	case Stopping:
		if a.stopTimer != nil {
			a.stopTimer.Stop()
			a.stopTimer = nil
		}
		a.setStatus(func(s *Status) { s.State = Stopped })
		a.log.Writef("stopped")
		for _, w := range a.stopWaiters {
			w <- nil
		}
		a.stopWaiters = nil
	case Starting:
		err := a.pendingErr
		if err == nil {
			err = fmt.Errorf("exited before becoming healthy: %s", desc)
		}
		a.setStatus(func(s *Status) { s.LastError = err.Error() })
		a.failure(err)
	case Running:
		err := fmt.Errorf("exited unexpectedly: %s", desc)
		a.setStatus(func(s *Status) { s.LastError = err.Error() })
		a.failure(err)
	}
}

// failure applies the restart policy after a failed start or crash.
func (a *App) failure(err error) {
	a.replyStart(err)
	if a.desired != Running {
		a.setStatus(func(s *Status) { s.State = Stopped })
		return
	}
	now := time.Now()
	if h := a.Status().HealthyAt; !h.IsZero() && now.Sub(h) >= a.policy.StableAfter {
		a.consecutive = 0
	}
	a.exits = append(a.exits, now)
	cut := 0
	for cut < len(a.exits) && now.Sub(a.exits[cut]) > a.policy.CrashWindow {
		cut++
	}
	a.exits = a.exits[cut:]
	if len(a.exits) >= a.policy.CrashLimit {
		a.setStatus(func(s *Status) { s.State, s.NextStart = Crashed, time.Time{} })
		a.log.Writef("crashed: %d failures within %s; not restarting until started manually", len(a.exits), a.policy.CrashWindow)
		a.slog.Error("app crashed; automatic restarts stopped", "failures", len(a.exits))
		return
	}
	delay := a.policy.BackoffMin << a.consecutive
	if delay > a.policy.BackoffMax || delay <= 0 {
		delay = a.policy.BackoffMax
	}
	a.consecutive++
	a.backoff = time.NewTimer(delay)
	next := now.Add(delay)
	a.setStatus(func(s *Status) { s.State, s.NextStart = Backoff, next; s.Restarts++ })
	a.log.Writef("restarting in %s", delay)
}

func (a *App) replyStart(err error) {
	for _, w := range a.startWaiters {
		w <- err
	}
	a.startWaiters = nil
}

func (a *App) killGroup() {
	if err := a.group.Kill(); err != nil {
		a.slog.Error("killing app cgroup failed", "err", err)
	}
}

// checkOOM records new OOM kills; it reports whether any occurred.
func (a *App) checkOOM() bool {
	st, err := a.group.Stats()
	if err != nil || st.OOMKills <= a.lastOOM {
		return false
	}
	delta := st.OOMKills - a.lastOOM
	a.lastOOM = st.OOMKills
	a.setStatus(func(s *Status) { s.OOMKills += delta })
	a.log.Writef("OOM: kernel killed %d process(es) at the %s memory limit", delta, formatBytes(a.Spec().Limits.MemoryMax))
	a.slog.Warn("app hit its memory limit", "oom_kills", delta)
	return true
}

func describeExit(ev exitEvent) string {
	ps := ev.state
	if ps == nil {
		if ev.err != nil {
			return ev.err.Error()
		}
		return "unknown"
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return "killed by signal " + unix.SignalName(ws.Signal())
	}
	return "exit code " + strconv.Itoa(ps.ExitCode())
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
