package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/im-anuvaad/phpboat/internal/apps"
	"github.com/im-anuvaad/phpboat/internal/auth"
	"github.com/im-anuvaad/phpboat/internal/backup"
	"github.com/im-anuvaad/phpboat/internal/buildinfo"
	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/cron"
	"github.com/im-anuvaad/phpboat/internal/deployer"
	"github.com/im-anuvaad/phpboat/internal/edge"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/metrics"
	"github.com/im-anuvaad/phpboat/internal/php"
	"github.com/im-anuvaad/phpboat/internal/secrets"
	"github.com/im-anuvaad/phpboat/internal/store"
	"github.com/im-anuvaad/phpboat/internal/supervisor"
	"github.com/im-anuvaad/phpboat/internal/users"
	"github.com/im-anuvaad/phpboat/internal/web"
)

func runServe(stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := serve(log); err != nil {
		log.Error("phpboat stopped with an error", "err", err)
		return 1
	}
	return 0
}

func serve(log *slog.Logger) error {
	if os.Geteuid() != 0 {
		return errors.New("phpboat serve must run as root (it manages users and cgroups)")
	}
	log.Info("starting", "version", buildinfo.String())
	tuneMemory()

	lay := layout.Default()
	if err := layout.EnsureBase(lay); err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(layout.MasterKey)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := os.Chmod(lay.DBPath(), 0o600); err != nil {
		return err
	}
	ver, _ := st.SchemaVersion(ctx)
	log.Info("state database ready", "path", lay.DBPath(), "schema", ver)

	cg, err := cgroup.Setup("")
	if err != nil {
		return err
	}
	log.Info("cgroup tree ready", "root", cg.Root(), "controllers", keys(cg.Controllers()))
	if cleaned, err := cg.ReapStale(ctx); err != nil {
		return err
	} else if len(cleaned) > 0 {
		log.Warn("killed leftover processes from a previous run", "groups", cleaned)
	}

	phpRT := detectPHP(log)
	sup := supervisor.New(supervisor.Deps{Layout: lay, Cgroups: cg, Log: log, Web: supervisor.Web{
		PHP:       phpRT.get,
		ClientIP:  edge.ClientIP,
		ErrorPage: edge.ErrorPage,
	}})
	dep, err := deployer.New(ctx, deployer.Deps{
		Layout: lay, Store: st, Secrets: box, Supervisor: sup, Cgroups: cg, PHP: phpRT.get, Log: log,
	})
	if err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	authSvc := auth.New(st)
	edgeMgr, err := newEdge(ctx, log, lay, st, box, sup, dep, authSvc)
	if err != nil {
		return err
	}

	bk := newBackups(lay, st, box, sup, dep, log)
	iv, rt := bk.Policy()
	log.Info("backups", "every", iv, "keep", rt)
	if err := bk.Init(ctx); err != nil {
		return err
	}
	dep.PreDeploy = func(ctx context.Context, name string, lg *logs.Log) error {
		_, err := bk.Run(ctx, name, backup.KindPreDeploy, false, lg)
		switch {
		case errors.Is(err, backup.ErrEmpty):
			lg.Writef("pre-deploy backup: DATA_DIR is empty; nothing to back up")
			return nil
		case errors.Is(err, backup.ErrTooLarge):
			// Warn and deploy anyway: a data folder over the limit must not
			// block releases. The failed backup stays in the backups list.
			lg.Writef("warning: %v; the pre-deploy backup was skipped and the deploy continues", err)
			return nil
		}
		return err
	}

	// Cron: one scheduler for all apps. A deploy drains it before it stops an app.
	cronSched := cron.New(cron.Config{
		Store: st,
		Log:   log.With("component", "cron"),
		Apps: func() []cron.Process {
			var out []cron.Process
			for _, a := range sup.Apps() {
				out = append(out, a)
			}
			return out
		},
		Busy: dep.Busy,
	})
	dep.Cron = cronSched
	go cronSched.Run(runCtx)

	appSvc := apps.New(st, box, dep, sup, edgeMgr, edgeMgr.DashboardHost, log)
	appSvc.SetBackups(bk)
	if err := appSvc.Load(ctx); err != nil {
		return err
	}
	for _, rt := range appSvc.Routes() {
		if dh := edgeMgr.DashboardHost(); dh != "" && rt.Host == dh {
			log.Error("an app uses the dashboard domain; it is not routed", "app", rt.App, "domain", rt.Host)
		}
	}

	mc := &metrics.Collector{Store: st, Sup: sup, DataRoot: lay.Root, Log: log.With("component", "metrics"),
		Requests: edgeMgr.Router.Stats}

	dash := &web.Server{
		Auth: authSvc, Apps: appSvc, Dep: dep, Sup: sup, Edge: edgeMgr, Store: st,
		Backups: bk, Metrics: mc, PHP: phpRT.info,
		Thresholds: web.Thresholds{DiskPercent: 85, MemoryPercent: 90, CertDays: 14},
		Layout:     lay, Version: buildinfo.Version, Log: log.With("component", "web"),
	}
	h, err := dash.Handler()
	if err != nil {
		return err
	}
	edgeMgr.Router.Dashboard = h
	if edgeMgr.DashboardHost() == "" {
		log.Warn("no dashboard domain yet: the dashboard is only on the setup address (https://<server IP>)")
	}
	appSvc.RefreshEdge()
	edgeErr := make(chan error, 1)
	go func() { edgeErr <- edgeMgr.Serve(runCtx) }()
	edgeMgr.Run(runCtx)

	dep.Run(runCtx)
	bk.Schedule(runCtx)
	mc.Run(runCtx)
	go sup.StartAll(runCtx, func(name string) bool { return dep.DesiredRunning(runCtx, name) })
	go phpRT.watch(runCtx, log, sup, dep)
	go func() {
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-t.C:
				sup.CleanAllSessions(now)
			}
		}
	}()

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

loop:
	for {
		select {
		case <-usr1:
			logStatus(log, sup)
		case err := <-edgeErr:
			if err != nil {
				stop()
				dep.Stop()
				sup.StopAll(context.Background())
				return err
			}
		case <-runCtx.Done():
			break loop
		}
	}

	log.Info("shutting down: cancelling deployments")
	dep.Stop()
	waitUploads(log, bk, 20*time.Second)
	mc.Wait()
	log.Info("stopping apps")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.StopAll(stopCtx); err != nil {
		return fmt.Errorf("stopping apps: %w", err)
	}
	log.Info("all apps stopped; bye")
	return nil
}

// newBackups creates the backup service and connects it to the apps.
func newBackups(lay layout.Layout, st *store.Store, box *secrets.Box, sup *supervisor.Supervisor, dep *deployer.Deployer, log *slog.Logger) *backup.Service {
	return &backup.Service{
		Layout: lay, Store: st, Box: box, Log: log.With("component", "backup"),
		Hooks: backup.Hooks{
			Apps: func() []string {
				var names []string
				for _, a := range sup.Apps() {
					names = append(names, a.Spec().Name)
				}
				return names
			},
			Build: func(name string) string {
				if a := sup.Get(name); a != nil {
					return a.Status().Build
				}
				return ""
			},
			Reserve: func(name string) (func(), error) { return dep.Reserve(name, "a restore") },
			Stop: func(ctx context.Context, name string) (bool, error) {
				a := sup.Get(name)
				if a == nil {
					return false, fmt.Errorf("unknown app %q", name)
				}
				switch a.Status().State {
				case supervisor.Stopped, supervisor.Crashed:
					return false, nil
				}
				return true, a.Stop(ctx)
			},
			Start: func(ctx context.Context, name string) error { return dep.StartReserved(ctx, name) },
			Owner: func(name string) (int, int, error) {
				u, err := users.Lookup(name)
				return int(u.UID), int(u.GID), err
			},
		},
	}
}

// waitUploads gives background backup uploads a moment to finish.
func waitUploads(log *slog.Logger, bk *backup.Service, max time.Duration) {
	done := make(chan struct{})
	go func() { bk.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(max):
		log.Warn("backup uploads still running at shutdown; they will be retried after the restart")
	}
}

// newEdge creates the edge manager and its router (not started yet).
func newEdge(ctx context.Context, log *slog.Logger, lay layout.Layout, st *store.Store, box *secrets.Box,
	sup *supervisor.Supervisor, dep *deployer.Deployer, au *auth.Auth) (*edge.Manager, error) {
	m, err := edge.NewManager(ctx, edge.Config{
		Listen: ":443", AOP: true, DataRoot: lay.Root,
		SetupPending: func() bool { return !au.SetupUntil().IsZero() },
	}, st, box, log)
	if err != nil {
		return nil, err
	}
	m.Router = &edge.Router{
		Log: log,
		Handler: func(name string) http.Handler {
			if a := sup.Get(name); a != nil {
				return a.Handler()
			}
			return nil
		},
		State: func(name string) edge.Availability {
			if dep.Deploying(name) {
				return edge.Deploying
			}
			a := sup.Get(name)
			if a == nil {
				return edge.NotRunning
			}
			switch a.Status().State {
			case supervisor.Running:
				return edge.Available
			case supervisor.Starting:
				return edge.Starting
			}
			return edge.NotRunning
		},
	}
	m.Router.SetDashboardHost(m.DashboardHost())
	return m, nil
}

// phpRuntime is the host's PHP, detected at start and again whenever it
// was missing (the installer may add it while phpboat runs).
type phpRuntime struct {
	mu  sync.Mutex
	rt  php.Runtime
	err error
	pkg string // installed php-fpm package version; set at detection and on each PHP update
	log *slog.Logger
}

func detectPHP(log *slog.Logger) *phpRuntime {
	p := &phpRuntime{log: log}
	p.get()
	return p
}

func (p *phpRuntime) get() (php.Runtime, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil && p.rt.FPM != "" {
		return p.rt, nil
	}
	p.rt, p.err = php.Detect("")
	if p.err != nil {
		p.log.Error("no PHP: apps cannot start", "err", p.err)
	} else {
		p.pkg = p.rt.Build(context.Background())
		p.log.Info("PHP found", "version", p.rt.Version, "fpm", p.rt.FPM, "build", p.pkg)
	}
	return p.rt, p.err
}

// info is the host's PHP and its installed package version, from the cache
// (the dashboard reads it on every page; it does not call dpkg).
func (p *phpRuntime) info() (php.Runtime, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return php.Runtime{}, ""
	}
	return p.rt, p.pkg
}

// watch restarts the running apps one at a time when apt updates PHP:
// running FPM masters keep the old code until they restart (docs/architecture.md §3).
func (p *phpRuntime) watch(ctx context.Context, log *slog.Logger, sup *supervisor.Supervisor, dep *deployer.Deployer) {
	rt, err := p.get()
	for err != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		rt, err = p.get()
	}
	rt.Watch(ctx, 10*time.Minute, func(old, cur string) {
		log.Warn("PHP was updated; restarting the running apps one at a time", "from", old, "to", cur)
		p.mu.Lock()
		p.pkg = cur
		p.mu.Unlock()
		for _, a := range sup.Apps() {
			if a.Status().State != supervisor.Running {
				continue
			}
			name := a.Spec().Name
			a.Log().Writef("PHP was updated (%s → %s); restarting to use it", old, cur)
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := dep.Restart(rctx, name); err != nil {
				a.Log().Writef("restart after the PHP update failed: %v (a deploy also restarts it)", err)
			}
			cancel()
		}
	})
}

// logStatus prints every app's state (send SIGUSR1 to phpboat).
func logStatus(log *slog.Logger, sup *supervisor.Supervisor) {
	for _, a := range sup.Apps() {
		s := a.Status()
		attrs := []any{"app", a.Spec().Name, "state", s.State, "pid", s.PID, "build", s.Build,
			"restarts", s.Restarts, "oom_kills", s.OOMKills}
		if s.LastExit != "" {
			attrs = append(attrs, "last_exit", s.LastExit)
		}
		if s.LastError != "" {
			attrs = append(attrs, "last_error", s.LastError)
		}
		if st, err := a.Group().Stats(); err == nil {
			attrs = append(attrs, "mem_bytes", st.MemoryCurrent, "pids", st.PidsCurrent, "cpu_usec", st.CPUUsageUsec)
		}
		log.Info("status", attrs...)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tuneMemory keeps phpboat inside its 30 MB resident budget: a
// soft heap limit makes the GC run more often before the heap grows, and
// freed memory is handed back to the kernel every 2 minutes instead of
// being kept for reuse. GOMEMLIMIT / GOGC in the environment override.
func tuneMemory() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(12 << 20)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
	go func() {
		for range time.Tick(2 * time.Minute) {
			debug.FreeOSMemory()
		}
	}()
}
