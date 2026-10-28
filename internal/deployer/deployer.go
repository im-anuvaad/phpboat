// Package deployer implements the deploy pipeline (docs/plan.md §5). A commit
// of the app's branch is fetched as a source archive, checked against
// phpboat.toml, and prepared into a build: Composer runs in its own cgroup,
// and the result is read-only. The old build serves until the switch. The
// switch stops the old build, backs up the data, links the persist folders,
// starts the new build (its before_start scripts run first) and waits for it
// to be healthy. If that fails, the previous build comes back.
//
// All deploys and rollbacks on the host run one at a time from a single
// queue.
package deployer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/github"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/php"
	"github.com/im-anuvaad/phpboat/internal/secrets"
	"github.com/im-anuvaad/phpboat/internal/store"
	"github.com/im-anuvaad/phpboat/internal/supervisor"
	"github.com/im-anuvaad/phpboat/internal/users"
)

// Retention.
const (
	KeepBuilds    = 3
	KeepBuildLogs = 20
)

// Settings keys.
const (
	SettingGitHubToken = "github_token"
	purposeGitHubToken = "settings:github_token"
)

// Limits and timeouts of a deploy.
const (
	maxTarball      = 200 << 20 // the source archive of a commit (docs/plan.md §5)
	commitsTTL      = time.Minute
	ListCommits     = 10 // commits the app page offers
	composeTimeout  = 10 * time.Minute
	platformTimeout = time.Minute
	// composeMemory is PHP's memory_limit for Composer. Its cgroup allows 256 MB.
	composeMemory = "192M"
	// activateTimeout bounds one switch: stop, backup, start, before_start
	// scripts (each up to 5 min) and the health check.
	activateTimeout = 15 * time.Minute
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidateCommit checks that s is a full commit SHA (lowercase hex).
func ValidateCommit(s string) error {
	if !shaRe.MatchString(s) {
		return fmt.Errorf("%q is not a full commit SHA (40 lowercase hex digits)", s)
	}
	return nil
}

// BuildID names the build of a commit: the first 12 hex digits of its SHA.
func BuildID(sha string) string { return sha[:12] }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// AppConfig is the deploy configuration of one app.
type AppConfig struct {
	Base   app.Spec // name, domain, concurrency, env, limits (no build fields)
	Repo   github.Repo
	Branch string // the branch whose commits are deployed
}

// Deps are the deployer's collaborators.
type Deps struct {
	Layout     layout.Layout
	Store      *store.Store
	Secrets    *secrets.Box
	Supervisor *supervisor.Supervisor
	Cgroups    *cgroup.Manager
	// PHP returns the host's PHP (Composer and before_start run with its CLI).
	PHP func() (php.Runtime, error)
	Log *slog.Logger
	// PreDeploy runs after the old build stopped and before the switch.
	// Phase 5 plugs the full-data backup in here. A returned error aborts the
	// deploy and restarts the old build.
	PreDeploy func(ctx context.Context, app string, log *logs.Log) error
	// Cron stops an app's scheduled jobs before the app is stopped for a
	// switch (docs/plan.md §5 step 7). Set by serve.go; nil means no jobs.
	Cron CronDrainer
}

// CronDrainer is implemented by the cron scheduler (internal/cron). The
// deployer needs only this call, so it does not import that package.
type CronDrainer interface {
	// Drain stops app's jobs and reports how many were running and how many
	// of them were killed.
	Drain(ctx context.Context, app string) (running, killed int)
}

// Deployer runs deployments.
type Deployer struct {
	Deps
	db  *store.Store
	log *slog.Logger

	mu        sync.Mutex
	apps      map[string]AppConfig
	pending   map[string]int64  // app -> queued or running deployment
	held      map[string]string // app -> reason (e.g. a restore) that blocks deployments
	deploying map[string]bool   // app is between "stop old" and "healthy/failed" (served 503)
	draining  map[string]bool   // cron jobs are being stopped before the app goes down
	commits   map[string]cachedCommits

	queue  chan job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type job struct {
	id   int64
	app  string
	kind string
	ref  string // a commit SHA (deploy, rebuild) or a build ID (rollback)
}

type cachedCommits struct {
	at   time.Time
	list []github.Commit
	err  error
}

// New creates a deployer, marks interrupted deployments as failed and
// cleans leftover downloads. Call Run to start the worker.
func New(ctx context.Context, d Deps) (*Deployer, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	dp := &Deployer{
		Deps:      d,
		db:        d.Store,
		log:       d.Log,
		apps:      map[string]AppConfig{},
		pending:   map[string]int64{},
		held:      map[string]string{},
		deploying: map[string]bool{},
		draining:  map[string]bool{},
		commits:   map[string]cachedCommits{},
		queue:     make(chan job, 64),
	}
	if n, err := dp.recoverInterrupted(ctx); err != nil {
		return nil, err
	} else if n > 0 {
		dp.log.Warn("marked deployments interrupted by a restart as failed", "count", n)
	}
	return dp, nil
}

// Register adds an app and its current build (if any) to the supervisor.
func (d *Deployer) Register(ctx context.Context, cfg AppConfig) (*supervisor.App, error) {
	spec := cfg.Base
	d.recoverWork(spec.Name)
	if cur := d.currentBuild(spec.Name); cur != "" {
		b, err := d.build(ctx, spec.Name, cur)
		if err != nil {
			d.log.Warn("current build has no record; app needs a deploy", "app", spec.Name, "build", cur, "err", err)
		} else {
			spec = d.specFor(cfg, b)
		}
	}
	a, err := d.Supervisor.Add(spec)
	if err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(d.Layout.DownloadsDir(spec.Name))
	for _, x := range entries {
		os.RemoveAll(filepath.Join(d.Layout.DownloadsDir(spec.Name), x.Name()))
	}
	d.mu.Lock()
	d.apps[spec.Name] = cfg
	d.mu.Unlock()
	return a, nil
}

// specFor is the supervisor spec of a build of the app.
func (d *Deployer) specFor(cfg AppConfig, b Build) app.Spec {
	s := cfg.Base
	s.BuildID = b.ID
	s.CommitSHA = b.CommitSHA
	s.BuildDir = d.Layout.BuildDir(s.Name, b.ID)
	s.Docroot = b.Manifest.Docroot
	s.HealthPath = b.Manifest.HealthPath
	s.PHP = b.Manifest.PHP
	s.BeforeStart = b.Manifest.BeforeStart
	s.Cron = b.Manifest.Cron
	return s
}

// currentBuild reads the `current` symlink ("" if none).
func (d *Deployer) currentBuild(appName string) string {
	t, err := os.Readlink(d.Layout.CurrentLink(appName))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// Deploying reports whether app is in the downtime window of a deploy
// (the router shows a 503 "deploying" page then).
func (d *Deployer) Deploying(appName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deploying[appName]
}

// Busy reports whether app is being drained, is in the downtime window of a
// deploy, or is reserved for a restore. Cron jobs don't start then.
func (d *Deployer) Busy(appName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deploying[appName] || d.draining[appName] || d.held[appName] != ""
}

// setFlag sets or clears one per-app flag of the deployer.
func (d *Deployer) setFlag(flags map[string]bool, name string, on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if on {
		flags[name] = true
	} else {
		delete(flags, name)
	}
}

// Deploy queues a deploy of commit sha of the app's branch.
func (d *Deployer) Deploy(ctx context.Context, appName, sha string) (int64, error) {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha == "" {
		return 0, errors.New("choose a commit to deploy")
	}
	if err := ValidateCommit(sha); err != nil {
		return 0, err
	}
	if d.currentBuild(appName) == BuildID(sha) {
		return 0, fmt.Errorf("commit %s is already running (use Restart to restart it, or Rebuild to build it again)", short(sha))
	}
	return d.enqueue(ctx, appName, KindDeploy, sha)
}

// Rebuild queues a new build of the deployed commit: the source is fetched
// again and Composer runs again (e.g. after a Composer outage or a changed
// COMPOSER_AUTH).
func (d *Deployer) Rebuild(ctx context.Context, appName string) (int64, error) {
	cur := d.currentBuild(appName)
	if cur == "" {
		return 0, errors.New("nothing is deployed yet: deploy a commit first")
	}
	b, err := d.build(ctx, appName, cur)
	if err != nil {
		return 0, err
	}
	return d.enqueue(ctx, appName, KindRebuild, b.CommitSHA)
}

// GitHubCommits lists the newest commits of the app's branch, cached for a
// minute (the app page refreshes every 5 s).
func (d *Deployer) GitHubCommits(ctx context.Context, appName string) ([]github.Commit, error) {
	d.mu.Lock()
	cfg, ok := d.apps[appName]
	c, cached := d.commits[appName]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown app %q", appName)
	}
	if cached && time.Since(c.at) < commitsTTL {
		return c.list, c.err
	}
	token, err := d.GitHubToken(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	list, err := (&github.API{Token: token}).Commits(cctx, cfg.Repo, cfg.Branch, ListCommits)
	d.mu.Lock()
	d.commits[appName] = cachedCommits{at: time.Now(), list: list, err: err}
	d.mu.Unlock()
	return list, err
}

// ForgetCommits drops the cached commit lists of app ("" = every app).
func (d *Deployer) ForgetCommits(appName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if appName == "" {
		d.commits = map[string]cachedCommits{}
	} else {
		delete(d.commits, appName)
	}
}

// Rollback queues a switch to a kept build (no download).
func (d *Deployer) Rollback(ctx context.Context, appName, buildID string) (int64, error) {
	if buildID == "" {
		return 0, errors.New("choose a build to roll back to")
	}
	if _, err := d.build(ctx, appName, buildID); err != nil {
		return 0, err
	}
	if d.currentBuild(appName) == buildID {
		return 0, fmt.Errorf("build %s is already current", buildID)
	}
	return d.enqueue(ctx, appName, KindRollback, buildID)
}

func (d *Deployer) enqueue(ctx context.Context, appName, kind, ref string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.apps[appName]; !ok {
		return 0, fmt.Errorf("unknown app %q", appName)
	}
	if id, ok := d.pending[appName]; ok {
		return 0, fmt.Errorf("deployment #%d of %s is still in progress", id, appName)
	}
	if why, ok := d.held[appName]; ok {
		return 0, fmt.Errorf("%s: %s is in progress; deploy when it has finished", appName, why)
	}
	if d.ctx == nil || d.ctx.Err() != nil {
		return 0, errors.New("deployer is not running")
	}
	buildID, commit := BuildID(ref), ref
	if kind == KindRollback {
		b, err := d.build(ctx, appName, ref)
		if err != nil {
			return 0, err
		}
		buildID, commit = b.ID, b.CommitSHA
	}
	id, err := d.insertDeployment(ctx, appName, kind, buildID, commit)
	if err != nil {
		return 0, err
	}
	select {
	case d.queue <- job{id: id, app: appName, kind: kind, ref: ref}:
	default:
		d.finish(id, errors.New("deploy queue is full"))
		return 0, errors.New("deploy queue is full")
	}
	d.pending[appName] = id
	return id, nil
}

// Run starts the worker. Stop cancels the running job and waits.
func (d *Deployer) Run(ctx context.Context) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.ctx.Done():
				for {
					select {
					case j := <-d.queue:
						d.finish(j.id, errors.New("cancelled: phpboat is shutting down"))
					default:
						return
					}
				}
			case j := <-d.queue:
				d.runJob(j)
			}
		}
	}()
}

// Stop cancels the current job and waits for the worker to exit.
func (d *Deployer) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.wg.Wait()
}

func (d *Deployer) runJob(j job) {
	d.mu.Lock()
	cfg := d.apps[j.app]
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, j.app)
		delete(d.deploying, j.app)
		d.mu.Unlock()
	}()

	lg, err := logs.Open(d.Layout.BuildLog(j.app, j.id), logs.Options{MaxSize: 256 << 20, Keep: 1, RingSize: 16})
	if err != nil {
		d.finish(j.id, err)
		return
	}
	start := time.Now()
	d.setStatus(j.id, StatusPreparing)
	d.log.Info("deployment started", "app", j.app, "id", j.id, "kind", j.kind)

	if j.kind == KindRollback {
		lg.Writef("rollback of %s to build %s", j.app, j.ref)
		err = d.rollback(d.ctx, cfg, j, lg)
	} else {
		lg.Writef("%s of %s: commit %s on branch %s of %s", j.kind, j.app, short(j.ref), cfg.Branch, cfg.Repo)
		err = d.deploy(d.ctx, cfg, j, lg)
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		lg.Writef("FAILED after %s: %v", took, err)
		d.log.Error("deployment failed", "app", j.app, "id", j.id, "err", err)
	} else {
		lg.Writef("SUCCEEDED in %s", took)
		d.log.Info("deployment succeeded", "app", j.app, "id", j.id, "took", took)
	}
	lg.Close()
	d.finish(j.id, err)
	d.ForgetCommits(j.app) // the page shows fresh commits after a deploy
	d.pruneBuildLogs(j.app)
}

// GitHubToken returns the stored GitHub token ("" if none).
func (d *Deployer) GitHubToken(ctx context.Context) (string, error) {
	v, ok, err := d.Store.GetSetting(ctx, SettingGitHubToken)
	if err != nil || !ok {
		return "", err
	}
	b, err := d.Secrets.Open(v, purposeGitHubToken)
	if err != nil {
		return "", fmt.Errorf("stored GitHub token: %w", err)
	}
	return string(b), nil
}

// SetGitHubToken stores the account-level GitHub token encrypted
// (empty removes it).
func (d *Deployer) SetGitHubToken(ctx context.Context, token string) error {
	if token == "" {
		_, err := d.Store.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingGitHubToken)
		return err
	}
	sealed, err := d.Secrets.Seal([]byte(token), purposeGitHubToken)
	if err != nil {
		return err
	}
	return d.Store.SetSetting(ctx, SettingGitHubToken, sealed)
}

// deploy brings commit j.ref live. A kept build of the commit is switched to
// without a download. Otherwise the commit is prepared (the old build keeps
// serving) and activated; the new build replaces the old one only in the
// switch, and activate restores the old one if the new one fails.
func (d *Deployer) deploy(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) error {
	name := cfg.Base.Name
	id := BuildID(j.ref)
	d.setBuild(j.id, id, j.ref)

	if j.kind == KindDeploy {
		if kept, err := d.build(ctx, name, id); err == nil && d.buildIntact(kept) {
			lg.Writef("build %s is still kept on this server: switching to it, no download", id)
			return d.activate(ctx, cfg, kept, j.id, lg, nil)
		}
	}
	b, work, err := d.prepare(ctx, cfg, j, lg)
	if err != nil {
		return err
	}
	in := &installation{d: d, b: b, work: work}
	defer func() { os.RemoveAll(work) }() // no-op once the build was moved into place
	if err := d.activate(ctx, cfg, b, j.id, lg, in); err != nil {
		in.undo()
		return err
	}
	in.commit()
	d.pruneBuilds(name, lg)
	return nil
}

func (d *Deployer) rollback(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) error {
	b, err := d.build(ctx, cfg.Base.Name, j.ref)
	if err != nil {
		return err
	}
	d.setBuild(j.id, b.ID, b.CommitSHA)
	if !d.buildIntact(b) {
		return fmt.Errorf("the files of build %s are missing on this server: deploy commit %s again instead", b.ID, short(b.CommitSHA))
	}
	return d.activate(ctx, cfg, b, j.id, lg, nil)
}

// buildIntact reports whether a kept build's docroot is still on disk.
func (d *Deployer) buildIntact(b Build) bool {
	fi, err := os.Stat(filepath.Join(d.Layout.BuildDir(b.App, b.ID), b.Manifest.Docroot))
	return err == nil && fi.IsDir()
}

// activate switches the app to build b. It stops the old build, runs the
// pre-deploy hook, moves the new build into place (in, if set), links the
// persist folders, switches `current` and starts the build (before_start
// runs first). If the new build does not start or is not healthy, the
// previous build is restored.
func (d *Deployer) activate(ctx context.Context, cfg AppConfig, b Build, id int64, lg *logs.Log, in *installation) error {
	name := cfg.Base.Name
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("app %s is not registered", name)
	}
	u, err := users.Lookup(name)
	if err != nil {
		return err
	}
	prevSpec := a.Spec()
	prevState := a.Status().State
	wasRunning := prevState == supervisor.Running || prevState == supervisor.Starting || prevState == supervisor.Backoff
	// Detached from the job context so a shutdown mid-switch still leaves
	// a consistent state.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activateTimeout)
	defer cancel()

	// Cron jobs stop first, while the app keeps serving: no job starts from
	// here on (Busy), running ones get 30 s, then they are killed. Only after
	// that does the app go into its downtime window.
	d.setFlag(d.draining, name, true)
	defer d.setFlag(d.draining, name, false)
	if d.Cron != nil {
		if running, killed := d.Cron.Drain(sctx, name); running > 0 {
			lg.Writef("cron: %d job(s) were running; %d were still running after the grace period and were killed", running, killed)
		}
	}

	d.setStatus(id, StatusDeploying)
	d.setFlag(d.deploying, name, true)
	downStart := time.Now()
	defer d.setFlag(d.deploying, name, false)
	if prevSpec.BuildID != "" {
		lg.Writef("stopping build %s (state %s)", prevSpec.BuildID, prevState)
	}
	if err := a.Stop(sctx); err != nil {
		return fmt.Errorf("stop old build: %w", err)
	}

	restorePrev := func(reason error) error {
		in.undo()
		if prevSpec.BuildID == "" {
			os.Remove(d.Layout.CurrentLink(name))
			a.Update(sctx, cfg.Base)
			lg.Writef("no previous build to go back to; the app stays stopped")
			return reason
		}
		if err := d.swapCurrent(name, prevSpec.BuildID); err != nil {
			return fmt.Errorf("%v; restoring the previous build also failed: %w", reason, err)
		}
		if err := a.Update(sctx, prevSpec); err != nil {
			return fmt.Errorf("%v; restoring the previous build also failed: %w", reason, err)
		}
		if !wasRunning {
			lg.Writef("switched back to build %s (left stopped, as it was before)", prevSpec.BuildID)
			return fmt.Errorf("%w; switched back to build %s", reason, prevSpec.BuildID)
		}
		if err := a.Start(sctx); err != nil {
			lg.Writef("previous build %s also failed to start: %v", prevSpec.BuildID, err)
			return fmt.Errorf("%w; rolled back to build %s, which also failed to start: %v", reason, prevSpec.BuildID, err)
		}
		lg.Writef("rolled back: build %s is serving again", prevSpec.BuildID)
		return fmt.Errorf("%w; rolled back to build %s", reason, prevSpec.BuildID)
	}

	if d.PreDeploy != nil {
		if err := d.PreDeploy(sctx, name, lg); err != nil {
			return restorePrev(fmt.Errorf("pre-deploy backup failed: %w", err))
		}
	}
	if in != nil {
		lg.Writef("moving build %s into place", b.ID)
		if err := in.apply(sctx); err != nil {
			return restorePrev(fmt.Errorf("move build %s into place: %w", b.ID, err))
		}
	}
	if err := d.linkPersist(name, u, d.Layout.BuildDir(name, b.ID), b.Manifest.Persist, lg); err != nil {
		return restorePrev(fmt.Errorf("persist folders: %w", err))
	}
	if err := d.swapCurrent(name, b.ID); err != nil {
		return restorePrev(fmt.Errorf("switch build: %w", err))
	}
	if err := d.Layout.WipeTmp(name, u); err != nil {
		lg.Writef("warning: %v", err)
	}
	if err := a.Update(sctx, d.specFor(cfg, b)); err != nil {
		return restorePrev(err)
	}
	lg.Writef("starting build %s (before_start scripts first)", b.ID)
	if err := a.Start(sctx); err != nil {
		lg.Writef("build %s did not start: %v", b.ID, err)
		if e := a.Stop(sctx); e != nil {
			lg.Writef("warning: stopping the failed build: %v", e)
		}
		return restorePrev(fmt.Errorf("build %s failed to start or its health check: %w", b.ID, err))
	}
	lg.Writef("build %s is healthy; downtime %s", b.ID, time.Since(downStart).Round(10*time.Millisecond))
	if err := d.SetDesired(sctx, name, true); err != nil {
		lg.Writef("warning: %v", err)
	}
	return nil
}

// swapCurrent atomically points apps/<app>/current at builds/<id>.
func (d *Deployer) swapCurrent(appName, buildID string) error {
	link := d.Layout.CurrentLink(appName)
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("builds", buildID), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// removeBuild deletes a build that is not current, with its database row.
func (d *Deployer) removeBuild(appName, id string, lg *logs.Log) {
	if d.currentBuild(appName) == id {
		return
	}
	if err := os.RemoveAll(d.Layout.BuildDir(appName, id)); err != nil {
		lg.Writef("warning: removing build %s: %v", id, err)
	}
	if err := d.deleteBuild(context.Background(), appName, id); err != nil {
		lg.Writef("warning: forgetting build %s: %v", id, err)
	}
}

// pruneBuilds keeps the current build plus the newest others, up to
// KeepBuilds in total, and deletes stray build directories.
func (d *Deployer) pruneBuilds(appName string, lg *logs.Log) {
	bs, err := d.Builds(context.Background(), appName)
	if err != nil {
		lg.Writef("warning: listing builds: %v", err)
		return
	}
	keep := map[string]bool{}
	for _, b := range bs {
		if b.Current {
			keep[b.ID] = true
		}
	}
	for _, b := range bs {
		if len(keep) >= KeepBuilds {
			break
		}
		keep[b.ID] = true
	}
	for _, b := range bs {
		if !keep[b.ID] {
			lg.Writef("pruning old build %s", b.ID)
			d.removeBuild(appName, b.ID, lg)
		}
	}
	entries, _ := os.ReadDir(d.Layout.BuildsDir(appName))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(d.Layout.BuildsDir(appName), e.Name()))
		}
	}
}

func (d *Deployer) pruneBuildLogs(appName string) {
	dir := d.Layout.BuildLogsDir(appName)
	entries, _ := os.ReadDir(dir)
	var ids []int64
	for _, e := range entries {
		base := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".1"), ".log")
		if n, err := strconv.ParseInt(base, 10, 64); err == nil && !containsID(ids, n) {
			ids = append(ids, n)
		}
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] > ids[k] })
	for _, id := range ids[min(len(ids), KeepBuildLogs):] {
		os.Remove(d.Layout.BuildLog(appName, id))
		os.Remove(d.Layout.BuildLog(appName, id) + ".1")
	}
}

func containsID(ids []int64, n int64) bool {
	for _, x := range ids {
		if x == n {
			return true
		}
	}
	return false
}

// Apps returns the registered app names (sorted).
func (d *Deployer) Apps() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.apps))
	for n := range d.apps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Config returns an app's deploy configuration.
func (d *Deployer) Config(appName string) (AppConfig, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.apps[appName]
	return c, ok
}

// Pending returns the queued or running deployment of app (0 if none).
func (d *Deployer) Pending(appName string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending[appName]
}
