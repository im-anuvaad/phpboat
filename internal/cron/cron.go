// Package cron runs the [[cron]] jobs of apps (docs/app-contract.md §8).
//
// A ticker wakes at the start of every minute. For each job whose schedule
// matches that minute, the job runs in its app, unless the app is not running,
// is being deployed or restored, or the job's previous run is still going.
// Every run and every skipped run is recorded in cron_runs. Output of a run
// goes to the app log, each line tagged cron:<n>.
//
// Jobs that are due while phpboat is down do not run later, as with system cron.
package cron

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/cron/schedule"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/phpcli"
	"github.com/im-anuvaad/phpboat/internal/store"
)

// DrainGrace is how long a deploy lets running jobs finish before it kills them.
const DrainGrace = 30 * time.Second

// killWait bounds the wait for killed jobs to record how they ended.
const killWait = 15 * time.Second

// Process is one app as the scheduler sees it. *supervisor.App implements it.
type Process interface {
	Spec() app.Spec
	Running() bool
	Log() *logs.Log
	// RunCron runs one job's script; the output is logged with prefix.
	RunCron(ctx context.Context, prefix, script string) (phpcli.Result, error)
}

// Config connects the scheduler to the rest of phpboat.
type Config struct {
	Store *store.Store
	// Apps lists the supervised apps. It is called on every tick.
	Apps func() []Process
	// Busy reports whether an app is in its deploy downtime or reserved for a
	// restore. No job starts then.
	Busy func(name string) bool
	Log  *slog.Logger
}

// key identifies a job: the app and the 1-based position of its [[cron]] table.
type key struct {
	app string
	job int
}

// run is a job in progress.
type run struct {
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	drained bool          // killed by Drain (guarded by Scheduler.mu)
	done    chan struct{} // closed once the run is recorded
}

// Scheduler runs the jobs of all apps.
type Scheduler struct {
	cfg     Config
	mu      sync.Mutex
	running map[key]*run
	last    time.Time // the last minute ticked; only Run touches it
}

// New creates a scheduler. Call Run to start it.
func New(cfg Config) *Scheduler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Apps == nil {
		cfg.Apps = func() []Process { return nil }
	}
	if cfg.Busy == nil {
		cfg.Busy = func(string) bool { return false }
	}
	return &Scheduler{cfg: cfg, running: map[key]*run{}}
}

// Run ticks at the start of every minute until ctx ends. Jobs in progress
// are cancelled with ctx.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		s.tick(ctx, time.Now().UTC().Truncate(time.Minute))
	}
}

// tick starts or skips the jobs that match minute.
func (s *Scheduler) tick(ctx context.Context, minute time.Time) {
	if !minute.After(s.last) {
		return
	}
	s.last = minute
	for _, p := range s.cfg.Apps() {
		spec := p.Spec()
		for i, j := range spec.Cron {
			sched, err := j.Parsed()
			if err != nil {
				s.cfg.Log.Error("cron job has an invalid schedule", "app", spec.Name, "job", i+1, "err", err)
				continue
			}
			if sched.Matches(minute) {
				s.dispatch(ctx, p, spec.Name, i+1, j, sched, minute)
			}
		}
	}
}

// dispatch starts a due job, or records it as skipped. The checks and the
// registration happen under one lock, so Drain sees every run that started.
func (s *Scheduler) dispatch(ctx context.Context, p Process, name string, n int, j app.CronJob, sched schedule.Schedule, minute time.Time) {
	k := key{app: name, job: n}
	s.mu.Lock()
	if !p.Running() {
		// A stopped or crashed app has nothing to run. Recording a skip every
		// minute would fill the history with nothing.
		s.mu.Unlock()
		return
	}
	var reason string
	switch {
	case s.cfg.Busy(name):
		reason = "the app is being deployed or restored"
	case s.running[k] != nil:
		reason = "the previous run is still going"
	}
	var r *run
	if reason == "" {
		rctx, cancel := context.WithCancel(ctx)
		r = &run{ctx: rctx, cancel: cancel, started: time.Now(), done: make(chan struct{})}
		s.running[k] = r
	}
	s.mu.Unlock()

	if reason != "" {
		now := time.Now()
		s.record(Entry{App: name, Job: n, StartedAt: now, FinishedAt: now, Status: StatusSkipped, ExitCode: -1, Note: reason})
		return
	}
	go s.execute(ctx, p, k, j, sched, minute, r)
}

// execute runs one job and records how it ended.
func (s *Scheduler) execute(parent context.Context, p Process, k key, j app.CronJob, sched schedule.Schedule, minute time.Time, r *run) {
	lg := p.Log()
	timeout := j.Limit()
	if timeout == 0 {
		timeout = app.MaxCronTimeout
		if next := sched.Next(minute); !next.IsZero() {
			timeout = min(next.Sub(minute), app.MaxCronTimeout)
		}
	}
	lg.Writef("cron:%d start: php %s, timeout %s", k.job, j.Run, shortDur(timeout))

	tctx, tcancel := context.WithTimeout(r.ctx, timeout)
	res, err := p.RunCron(tctx, "cron:"+strconv.Itoa(k.job)+" ", j.Run)
	tcancel()
	end := time.Now()

	s.mu.Lock()
	delete(s.running, k)
	drained := r.drained
	s.mu.Unlock()
	r.cancel()

	status, exit, note := classify(res, err, drained, parent.Err() != nil, timeout)
	took := end.Sub(r.started).Round(100 * time.Millisecond)
	code := "no exit code"
	if exit >= 0 {
		code = "exit code " + strconv.Itoa(exit)
	}
	detail := ""
	if note != "" {
		detail = " (" + note + ")"
	}
	lg.Writef("cron:%d end: %s, %s, took %s%s", k.job, status, code, took, detail)

	s.record(Entry{App: k.app, Job: k.job, StartedAt: r.started, FinishedAt: end, Status: status, ExitCode: exit, Note: note})
	close(r.done)
}

// classify turns the outcome of a run into its history status, exit code
// (-1 if none) and note.
func classify(res phpcli.Result, err error, drained, shutdown bool, timeout time.Duration) (string, int, string) {
	switch {
	case err != nil:
		return StatusFailed, -1, "could not start php: " + err.Error()
	case shutdown:
		return StatusKilled, -1, "phpboat stopped while the job ran"
	case drained:
		return StatusKilled, -1, "the app was deployed and the job did not finish within " + shortDur(DrainGrace)
	case res.TimedOut:
		return StatusTimeout, -1, "ran longer than " + shortDur(timeout) + ", killed"
	case res.ExitCode < 0:
		return StatusKilled, -1, "killed by a signal, e.g. because the app was stopped"
	case res.ExitCode == 0:
		return StatusOK, 0, ""
	}
	note := "exit code " + strconv.Itoa(res.ExitCode)
	if last := lastLine(res.Tail); last != "" {
		note += ": " + last
	}
	return StatusFailed, res.ExitCode, note
}

// Drain stops the jobs of app before a deploy stops the app. The deployer
// marks the app busy first, so no run starts after this call. Jobs still
// going get DrainGrace to finish; the rest are killed and recorded as killed.
// It reports how many jobs were going and how many it killed.
func (s *Scheduler) Drain(ctx context.Context, name string) (running, killed int) {
	s.mu.Lock()
	var runs []*run
	for k, r := range s.running {
		if k.app == name {
			runs = append(runs, r)
		}
	}
	s.mu.Unlock()
	if len(runs) == 0 {
		return 0, 0
	}
	s.cfg.Log.Info("waiting for cron jobs before the app is stopped", "app", name, "running", len(runs))

	all := make(chan struct{})
	go func() {
		for _, r := range runs {
			<-r.done
		}
		close(all)
	}()
	grace := time.NewTimer(DrainGrace)
	defer grace.Stop()
	select {
	case <-all:
		return len(runs), 0
	case <-grace.C:
	case <-ctx.Done():
	}

	s.mu.Lock()
	for _, r := range runs {
		select {
		case <-r.done:
		default:
			r.drained = true
			killed++
		}
	}
	s.mu.Unlock()
	for _, r := range runs {
		r.cancel() // kills the job's process group
	}
	select {
	case <-all:
	case <-time.After(killWait):
		s.cfg.Log.Error("cron jobs did not stop after being killed", "app", name)
	}
	return len(runs), killed
}

// record stores one history entry and keeps the newest KeepRuns of its job.
func (s *Scheduler) record(e Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var exit sql.NullInt64
	if e.ExitCode >= 0 {
		exit = sql.NullInt64{Int64: int64(e.ExitCode), Valid: true}
	}
	db := s.cfg.Store.Writer()
	_, err := db.ExecContext(ctx, `INSERT INTO cron_runs (app, job, started_at, finished_at, exit_code, status, note)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.App, e.Job, e.StartedAt.Unix(), e.FinishedAt.Unix(), exit, e.Status, e.Note)
	if err != nil {
		s.cfg.Log.Error("record cron run", "app", e.App, "job", e.Job, "err", err)
		return
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM cron_runs WHERE app = ? AND job = ?
		AND id NOT IN (SELECT id FROM cron_runs WHERE app = ? AND job = ? ORDER BY id DESC LIMIT ?)`,
		e.App, e.Job, e.App, e.Job, KeepRuns); err != nil {
		s.cfg.Log.Error("prune cron history", "app", e.App, "job", e.Job, "err", err)
	}
}

// shortDur formats a duration as 5m or 1h when it is a whole number of them.
func shortDur(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return d.Round(time.Millisecond).String()
}

// lastLine returns the last non-empty line of output, cut to a short note.
func lastLine(tail string) string {
	line := phpcli.LastLine(tail)
	if r := []rune(line); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return line
}
