// Package supervisor runs app processes: one goroutine ("actor") per app
// owns its process, state machine, health checks and restart policy.
//
// States: stopped → starting → running → stopping → stopped, plus
// backoff (waiting to restart after a failure) and crashed (gave up after
// too many failures; needs a manual start).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/php"
	"github.com/im-anuvaad/phpboat/internal/users"
)

// Deps are the supervisor's collaborators.
type Deps struct {
	Layout  layout.Layout
	Cgroups *cgroup.Manager
	Log     *slog.Logger
	Policy  Policy // zero value = DefaultPolicy
	Web     Web
}

// Web is how apps are run and served.
type Web struct {
	// PHP returns the host's PHP (php.Detect); an error keeps apps from
	// starting with that message.
	PHP func() (php.Runtime, error)
	// ClientIP is the visitor's IP (edge.ClientIP).
	ClientIP func(*http.Request) string
	// ErrorPage writes phpboat's error pages (404, 502, 503 ...).
	ErrorPage func(http.ResponseWriter, *http.Request, int)
}

// Supervisor owns all app actors.
type Supervisor struct {
	deps Deps
	mu   sync.Mutex
	apps map[string]*App
	// order preserves insertion order for sequential boot starts.
	order []string
}

// New creates a Supervisor.
func New(d Deps) *Supervisor {
	if d.Policy == (Policy{}) {
		d.Policy = DefaultPolicy
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if missing := d.Cgroups.Missing(); len(missing) > 0 {
		d.Log.Warn("cgroup controllers unavailable; the matching limits are NOT enforced", "missing", missing)
	}
	return &Supervisor{deps: d, apps: map[string]*App{}}
}

// Add prepares an app (system user, directories, cgroup + limits, log) and
// starts its actor. The app stays stopped until Start is called.
func (s *Supervisor) Add(spec app.Spec) (*App, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[spec.Name]; ok {
		return nil, fmt.Errorf("supervisor: app %q already exists", spec.Name)
	}

	u, err := users.Ensure(spec.Name)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Layout.EnsureApp(spec.Name, u); err != nil {
		return nil, err
	}
	g, err := s.deps.Cgroups.Group(cgroup.Apps, spec.Name)
	if err != nil {
		return nil, err
	}
	if err := applyLimits(g, spec.Limits); err != nil {
		return nil, err
	}
	lg, err := logs.Open(s.deps.Layout.AppLog(spec.Name), logs.Options{})
	if err != nil {
		return nil, err
	}

	a := newApp(spec, s.deps.Layout, u, g, lg, s.deps.Policy, s.deps.Log.With("app", spec.Name), s.deps.Web)
	s.apps[spec.Name] = a
	s.order = append(s.order, spec.Name)
	go a.loop()
	return a, nil
}

func applyLimits(g *cgroup.Group, l app.Limits) error {
	// No memory.high: without swap it cannot reclaim app memory and only
	// stalls the app (observed in testing); a clean OOM kill + restart is better.
	_, err := g.SetLimits(cgroup.Limits{
		MemoryMax: l.MemoryMax,
		NoSwap:    true, // predictable OOM at memory.max instead of swapping
		CPUMax:    l.CPUMax,
		PidsMax:   l.PidsMax,
	})
	return err
}

// Get returns the app or nil.
func (s *Supervisor) Get(name string) *App {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apps[name]
}

// Apps returns all apps in insertion order.
func (s *Supervisor) Apps() []*App {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*App, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.apps[n])
	}
	return out
}

// StartAll starts apps in parallel and returns once each has become
// healthy or failed. Apps without a build and apps for which want returns
// false are skipped. Failures are logged; failed apps keep retrying under
// the restart policy.
func (s *Supervisor) StartAll(ctx context.Context, want func(name string) bool) {
	// In parallel: an app whose before_start or health check takes long
	// (up to minutes) must not hold back the others.
	var wg sync.WaitGroup
	for _, a := range s.Apps() {
		if ctx.Err() != nil {
			break
		}
		if a.Spec().BuildID == "" {
			s.deps.Log.Info("app not started: never deployed", "app", a.Spec().Name)
			continue
		}
		if want != nil && !want(a.Spec().Name) {
			s.deps.Log.Info("app not started: it was stopped by the operator", "app", a.Spec().Name)
			continue
		}
		wg.Add(1)
		go func(a *App) {
			defer wg.Done()
			if err := a.Start(ctx); err != nil {
				s.deps.Log.Error("app failed to start", "app", a.Spec().Name, "err", err)
			}
		}(a)
	}
	wg.Wait()
}

// StopAll stops every app in parallel and shuts down the actors.
func (s *Supervisor) StopAll(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, a := range s.Apps() {
		wg.Add(1)
		go func(a *App) {
			defer wg.Done()
			if err := a.Stop(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", a.Spec().Name, err))
				mu.Unlock()
			}
			a.close()
		}(a)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Remove stops an app, shuts its actor down, removes its cgroup and
// forgets it. Its user and files are left to the caller.
func (s *Supervisor) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	a := s.apps[name]
	s.mu.Unlock()
	if a == nil {
		return nil
	}
	if err := a.Stop(ctx); err != nil {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	a.close()
	s.mu.Lock()
	delete(s.apps, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if err := a.group.Remove(); err != nil {
		s.deps.Log.Warn("removing app cgroup", "app", name, "err", err)
	}
	return nil
}
