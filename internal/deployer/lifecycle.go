package deployer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/supervisor"
)

// ErrBusy is returned while a deployment of the app is queued or running.
var ErrBusy = errors.New("a deployment of this app is in progress")

func (d *Deployer) busy(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.pending[name]; ok {
		return fmt.Errorf("%w (#%d)", ErrBusy, id)
	}
	return nil
}

// Reserve blocks deployments of an app (e.g. during a restore) until the
// returned unlock function is called. It fails if a deployment is
// queued or running.
func (d *Deployer) Reserve(name, reason string) (func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.pending[name]; ok {
		return nil, fmt.Errorf("%w (#%d); try again when it has finished", ErrBusy, id)
	}
	if why, ok := d.held[name]; ok {
		return nil, fmt.Errorf("%s is already in progress", why)
	}
	d.held[name] = reason
	return func() {
		d.mu.Lock()
		delete(d.held, name)
		d.mu.Unlock()
	}, nil
}

// UpdateConfig replaces an app's configuration. It applies to the next
// deploy and the next (re)start; running processes are not touched.
func (d *Deployer) UpdateConfig(cfg AppConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.apps[cfg.Base.Name]; !ok {
		return fmt.Errorf("unknown app %q", cfg.Base.Name)
	}
	d.apps[cfg.Base.Name] = cfg
	delete(d.commits, cfg.Base.Name) // the branch may have changed
	return nil
}

// Unregister forgets an app (it must have no pending deployment). The
// caller removes it from the supervisor.
func (d *Deployer) Unregister(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.pending[name]; ok {
		return fmt.Errorf("%w (#%d)", ErrBusy, id)
	}
	delete(d.apps, name)
	return nil
}

// Forget deletes an app's builds, deployments and desired state rows.
func (d *Deployer) Forget(ctx context.Context, name string) error {
	for _, q := range []string{
		`DELETE FROM builds WHERE app = ?`,
		`DELETE FROM deployments WHERE app = ?`,
		`DELETE FROM app_state WHERE app = ?`,
		`DELETE FROM cron_runs WHERE app = ?`,
	} {
		if _, err := d.db.Writer().ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	return nil
}

// apply pushes the current configuration into a stopped app.
func (d *Deployer) apply(ctx context.Context, name string, a *supervisor.App) error {
	cfg, ok := d.Config(name)
	if !ok {
		return nil // prebuilt dev app: nothing to apply
	}
	spec := cfg.Base
	if cur := d.currentBuild(name); cur != "" {
		b, err := d.build(ctx, name, cur)
		if err != nil {
			return err
		}
		spec = d.specFor(cfg, b)
	}
	return a.Update(ctx, spec)
}

// Start starts an app with its latest configuration.
func (d *Deployer) Start(ctx context.Context, name string) error {
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("unknown app %q", name)
	}
	if err := d.busy(name); err != nil {
		return err
	}
	switch a.Status().State {
	case supervisor.Stopped, supervisor.Crashed:
		if err := d.apply(ctx, name, a); err != nil {
			return err
		}
	}
	if err := a.Start(ctx); err != nil {
		if errors.Is(err, supervisor.ErrNotDeployed) {
			return err
		}
		d.SetDesired(ctx, name, true)
		return err
	}
	return d.SetDesired(ctx, name, true)
}

// StartReserved is Start for callers holding a Reserve (restores).
func (d *Deployer) StartReserved(ctx context.Context, name string) error {
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("unknown app %q", name)
	}
	switch a.Status().State {
	case supervisor.Stopped, supervisor.Crashed:
		if err := d.apply(ctx, name, a); err != nil {
			return err
		}
	}
	return a.Start(ctx)
}

// StopApp stops an app and keeps it stopped across phpboat restarts.
func (d *Deployer) StopApp(ctx context.Context, name string) error {
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("unknown app %q", name)
	}
	if err := d.busy(name); err != nil {
		return err
	}
	if err := d.SetDesired(ctx, name, false); err != nil {
		return err
	}
	return a.Stop(ctx)
}

// Restart stops the app, applies its latest configuration and starts it.
func (d *Deployer) Restart(ctx context.Context, name string) error {
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("unknown app %q", name)
	}
	if err := d.busy(name); err != nil {
		return err
	}
	if err := a.Stop(ctx); err != nil {
		return err
	}
	if err := d.apply(ctx, name, a); err != nil {
		return err
	}
	d.SetDesired(ctx, name, true)
	return a.Start(ctx)
}

// NeedsRestart reports whether a running app uses an older configuration
// (env vars, limits, PHP workers or domain changed since it started).
func (d *Deployer) NeedsRestart(name string) bool {
	cfg, ok := d.Config(name)
	a := d.Supervisor.Get(name)
	if !ok || a == nil {
		return false
	}
	st := a.Status().State
	if st == supervisor.Stopped || st == supervisor.Crashed {
		return false
	}
	sp := a.Spec()
	return !maps.Equal(sp.Env, cfg.Base.Env) || sp.Limits != cfg.Base.Limits || sp.Domain != cfg.Base.Domain ||
		sp.Concurrency != cfg.Base.Concurrency
}

// PurgeFiles removes an app's builds, logs and download workspaces.
// If keepData is false DATA_DIR is removed too; otherwise it is moved to
// <root>/deleted/<app>-<unix>/data and its path returned.
func (d *Deployer) PurgeFiles(name string, keepData bool, stamp int64) (string, error) {
	kept := ""
	if keepData {
		src := d.Layout.DataDir(name)
		if _, err := os.Stat(src); err == nil {
			dst := filepath.Join(d.Layout.Root, "deleted", fmt.Sprintf("%s-%d", name, stamp))
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return "", err
			}
			kept = filepath.Join(dst, "data")
			if err := os.Rename(src, kept); err != nil {
				return "", fmt.Errorf("keep data: %w", err)
			}
			// The app user is about to be deleted; give the files to root.
			filepath.WalkDir(kept, func(p string, _ os.DirEntry, err error) error {
				if err == nil {
					os.Lchown(p, 0, 0)
				}
				return nil
			})
		}
	}
	var errs []error
	for _, p := range []string{d.Layout.AppDir(name), d.Layout.DownloadsDir(name)} {
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	if d.Cgroups != nil {
		// The build group (Composer) is empty once the app is gone.
		if g, err := d.Cgroups.Group(cgroup.Builds, name); err == nil {
			if err := g.Remove(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return kept, errors.Join(errs...)
}
