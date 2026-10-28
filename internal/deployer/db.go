package deployer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/im-anuvaad/phpboat/internal/manifest"
)

// Deployment statuses.
const (
	StatusQueued    = "queued"
	StatusPreparing = "preparing" // fetching the commit, Composer, the build; the old build still serves
	StatusDeploying = "deploying" // the downtime window: stop, backup, switch, start
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Deployment kinds.
const (
	KindDeploy   = "deploy"   // a commit of the app's branch
	KindRebuild  = "rebuild"  // the deployed commit again, source and Composer included
	KindRollback = "rollback" // a kept build, switched to without a download
)

// Deployment is one row of the deploy history.
type Deployment struct {
	ID         int64     `json:"id"`
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	BuildID    string    `json:"build_id"`
	CommitSHA  string    `json:"commit_sha"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Done reports whether the deployment has finished.
func (d Deployment) Done() bool { return d.Status == StatusSucceeded || d.Status == StatusFailed }

// Build is a prepared build kept on the server (at most KeepBuilds per app).
type Build struct {
	App         string            `json:"app"`
	ID          string            `json:"id"` // the first 12 hex digits of CommitSHA
	CommitSHA   string            `json:"commit_sha"`
	Subject     string            `json:"subject"` // first line of the commit message
	Author      string            `json:"author"`
	CommittedAt time.Time         `json:"committed_at"`
	SHA256      string            `json:"sha256"` // of the source archive
	Manifest    manifest.Manifest `json:"manifest"`
	CreatedAt   time.Time         `json:"created_at"`
	Current     bool              `json:"current"`
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

const deploymentCols = `id, app, kind, status, build_id, commit_sha, error, created_at, started_at, finished_at`

func scanDeployment(s interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var c, st, f int64
	err := s.Scan(&d.ID, &d.App, &d.Kind, &d.Status, &d.BuildID, &d.CommitSHA, &d.Error, &c, &st, &f)
	d.CreatedAt, d.StartedAt, d.FinishedAt = fromUnix(c), fromUnix(st), fromUnix(f)
	return d, err
}

func (d *Deployer) insertDeployment(ctx context.Context, app, kind, buildID, commit string) (int64, error) {
	res, err := d.db.Writer().ExecContext(ctx,
		`INSERT INTO deployments (app, kind, status, build_id, commit_sha, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		app, kind, StatusQueued, buildID, commit, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("deployer: record deployment: %w", err)
	}
	return res.LastInsertId()
}

func (d *Deployer) setStatus(id int64, status string) {
	var err error
	if status == StatusPreparing {
		_, err = d.db.Writer().Exec(`UPDATE deployments SET status = ?, started_at = ? WHERE id = ?`, status, time.Now().Unix(), id)
	} else {
		_, err = d.db.Writer().Exec(`UPDATE deployments SET status = ? WHERE id = ?`, status, id)
	}
	if err != nil {
		d.log.Error("update deployment", "id", id, "err", err)
	}
}

func (d *Deployer) setBuild(id int64, buildID, commit string) {
	if _, err := d.db.Writer().Exec(`UPDATE deployments SET build_id = ?, commit_sha = ? WHERE id = ?`, buildID, commit, id); err != nil {
		d.log.Error("update deployment", "id", id, "err", err)
	}
}

func (d *Deployer) finish(id int64, err error) {
	status, msg := StatusSucceeded, ""
	if err != nil {
		status, msg = StatusFailed, err.Error()
	}
	if _, e := d.db.Writer().Exec(`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, msg, time.Now().Unix(), id); e != nil {
		d.log.Error("finish deployment", "id", id, "err", e)
	}
}

// Deployment returns one deployment.
func (d *Deployer) Deployment(ctx context.Context, id int64) (Deployment, error) {
	dep, err := scanDeployment(d.db.Reader().QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return dep, fmt.Errorf("deployment #%d not found", id)
	}
	return dep, err
}

// Deployments returns the newest deployments of app.
func (d *Deployer) Deployments(ctx context.Context, app string, limit int) ([]Deployment, error) {
	rows, err := d.db.Reader().QueryContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE app = ? ORDER BY id DESC LIMIT ?`, app, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func (d *Deployer) upsertBuild(ctx context.Context, b Build) error {
	m, err := json.Marshal(b.Manifest)
	if err != nil {
		return err
	}
	_, err = d.db.Writer().ExecContext(ctx, `INSERT INTO builds
		(app, id, commit_sha, subject, author, committed_at, sha256, manifest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app, id) DO UPDATE SET commit_sha = excluded.commit_sha, subject = excluded.subject,
			author = excluded.author, committed_at = excluded.committed_at, sha256 = excluded.sha256,
			manifest = excluded.manifest, created_at = excluded.created_at`,
		b.App, b.ID, b.CommitSHA, b.Subject, b.Author, unix(b.CommittedAt), b.SHA256, string(m), unix(b.CreatedAt))
	return err
}

func (d *Deployer) deleteBuild(ctx context.Context, app, id string) error {
	_, err := d.db.Writer().ExecContext(ctx, `DELETE FROM builds WHERE app = ? AND id = ?`, app, id)
	return err
}

// build loads one build row.
func (d *Deployer) build(ctx context.Context, app, id string) (Build, error) {
	bs, err := d.queryBuilds(ctx, `WHERE app = ? AND id = ?`, app, id)
	if err != nil {
		return Build{}, err
	}
	if len(bs) == 0 {
		return Build{}, fmt.Errorf("build %s of %s not found (only the last %d builds are kept)", id, app, KeepBuilds)
	}
	return bs[0], nil
}

// Builds lists the kept builds of app, newest first.
func (d *Deployer) Builds(ctx context.Context, app string) ([]Build, error) {
	bs, err := d.queryBuilds(ctx, `WHERE app = ? ORDER BY created_at DESC, id DESC`, app)
	if err != nil {
		return nil, err
	}
	cur := d.currentBuild(app)
	for i := range bs {
		bs[i].Current = bs[i].ID == cur
	}
	return bs, nil
}

func (d *Deployer) queryBuilds(ctx context.Context, where string, args ...any) ([]Build, error) {
	rows, err := d.db.Reader().QueryContext(ctx, `SELECT app, id, commit_sha, subject, author, committed_at, sha256, manifest, created_at
		FROM builds `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		var m string
		var committed, created int64
		if err := rows.Scan(&b.App, &b.ID, &b.CommitSHA, &b.Subject, &b.Author, &committed, &b.SHA256, &m, &created); err != nil {
			return nil, err
		}
		b.Manifest = manifest.Default()
		if err := json.Unmarshal([]byte(m), &b.Manifest); err != nil {
			return nil, fmt.Errorf("build %s: bad manifest column: %w", b.ID, err)
		}
		b.CommittedAt, b.CreatedAt = fromUnix(committed), fromUnix(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

// DesiredRunning reports whether app should run after a phpboat restart
// (default: yes).
func (d *Deployer) DesiredRunning(ctx context.Context, app string) bool {
	var s string
	err := d.db.Reader().QueryRowContext(ctx, `SELECT desired_state FROM app_state WHERE app = ?`, app).Scan(&s)
	return err != nil || s != "stopped"
}

// SetDesired records whether app should be running.
func (d *Deployer) SetDesired(ctx context.Context, app string, running bool) error {
	s := "stopped"
	if running {
		s = "running"
	}
	_, err := d.db.Writer().ExecContext(ctx, `INSERT INTO app_state (app, desired_state) VALUES (?, ?)
		ON CONFLICT (app) DO UPDATE SET desired_state = excluded.desired_state`, app, s)
	return err
}

// recoverInterrupted marks deployments interrupted by a phpboat restart as failed.
func (d *Deployer) recoverInterrupted(ctx context.Context) (int64, error) {
	res, err := d.db.Writer().ExecContext(ctx, `UPDATE deployments
		SET status = ?, error = 'interrupted: phpboat restarted during this deployment', finished_at = ?
		WHERE status IN (?, ?, ?)`, StatusFailed, time.Now().Unix(), StatusQueued, StatusPreparing, StatusDeploying)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
