package cron

import (
	"context"
	"database/sql"
	"time"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/store"
)

// Statuses of a run, as stored in cron_runs.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"  // exited non-zero, or could not start
	StatusTimeout = "timeout" // ran past its timeout and was killed
	StatusKilled  = "killed"  // stopped by a deploy, a shutdown, or a signal
	StatusSkipped = "skipped" // not started: see the note
)

// KeepRuns is how many runs of each job are kept. ShowRuns is how many the
// app page lists per job.
const (
	KeepRuns = 20
	ShowRuns = 10
)

// Entry is one row of cron_runs.
type Entry struct {
	App        string
	Job        int // 1-based position of the [[cron]] table in phpboat.toml
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string
	ExitCode   int    // -1 when the process did not exit normally
	Note       string // why it was skipped, killed or failed; "" for ok
}

// Duration is how long the run took (zero for skipped runs).
func (e Entry) Duration() time.Duration { return e.FinishedAt.Sub(e.StartedAt) }

// History returns the kept runs of app, newest first.
func History(ctx context.Context, st *store.Store, name string) ([]Entry, error) {
	rows, err := st.Reader().QueryContext(ctx, `SELECT job, started_at, finished_at, exit_code, status, note
		FROM cron_runs WHERE app = ? ORDER BY id DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var started, finished int64
		var exit sql.NullInt64
		if err := rows.Scan(&e.Job, &started, &finished, &exit, &e.Status, &e.Note); err != nil {
			return nil, err
		}
		e.App = name
		e.StartedAt, e.FinishedAt = time.Unix(started, 0), time.Unix(finished, 0)
		e.ExitCode = -1
		if exit.Valid {
			e.ExitCode = int(exit.Int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobView is one [[cron]] job as the app page shows it.
type JobView struct {
	Job      int
	Schedule string    // as written in phpboat.toml
	Run      string    // the PHP script
	Timeout  string    // the explicit timeout; "" = the default
	Next     time.Time // next run (UTC instant; shown in server time); zero if none
	Last     *Entry    // the newest run, nil if none
	Recent   []Entry   // newest first, at most ShowRuns
}

// Overview returns each job of an app with its next run and its recent runs.
// jobs are the jobs of the app's current build.
func Overview(ctx context.Context, st *store.Store, name string, jobs []app.CronJob, now time.Time) ([]JobView, error) {
	entries, err := History(ctx, st, name)
	if err != nil {
		return nil, err
	}
	views := make([]JobView, 0, len(jobs))
	for i, j := range jobs {
		v := JobView{Job: i + 1, Schedule: j.Schedule, Run: j.Run, Timeout: j.Timeout}
		if sched, err := j.Parsed(); err == nil {
			v.Next = sched.Next(now)
		}
		for _, e := range entries {
			if e.Job == v.Job && len(v.Recent) < ShowRuns {
				v.Recent = append(v.Recent, e)
			}
		}
		if len(v.Recent) > 0 {
			last := v.Recent[0]
			v.Last = &last
		}
		views = append(views, v)
	}
	return views, nil
}
