package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/im-anuvaad/phpboat/internal/buildinfo"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/s3"
	"github.com/im-anuvaad/phpboat/internal/secrets"
	"github.com/im-anuvaad/phpboat/internal/store"
)

// Kinds.
const (
	KindScheduled  = "scheduled"
	KindPreDeploy  = "pre-deploy"
	KindManual     = "manual"
	KindPreRestore = "pre-restore"
)

// Statuses.
const (
	StatusRunning = "running"
	StatusOK      = "ok"
	StatusFailed  = "failed"
)

// Policy (docs/plan.md §7). It is fixed.
const (
	Interval       = 3 * time.Hour
	Retention      = 48 * time.Hour
	KeepLocal      = 2
	uploadAttempts = 3
	// MaxContentBytes is the uncompressed size limit of one app's DATA_DIR.
	MaxContentBytes = 2 << 30
	// archiveSlack is the room kept for the archive's own overhead.
	archiveSlack = 16 << 20
)

// Settings keys.
const (
	SettingS3        = "s3_config"
	purposeS3        = "settings:s3"
	uploadRetryDelay = 3 * time.Second
	uploadPending    = "upload pending"
)

// ErrEmpty means DATA_DIR has nothing in it yet, so there is nothing to back up.
var ErrEmpty = errors.New("DATA_DIR is empty")

// Backup is one row of the backups table.
type Backup struct {
	ID           int64     `json:"id"`
	App          string    `json:"app"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	ObjectKey    string    `json:"object_key"`
	LocalPath    string    `json:"local_path"`
	Size         int64     `json:"size"`   // the archive, in bytes
	SHA256       string    `json:"sha256"` // of the archive
	Databases    int       `json:"databases"`
	PlainFiles   int       `json:"plain_files"`
	Dirs         int       `json:"dirs"`
	Links        int       `json:"links"`
	ContentBytes int64     `json:"content_bytes"` // uncompressed content
	BuildID      string    `json:"build_id"`      // the build that was running when the backup was taken
	Error        string    `json:"error"`
	CreatedAt    time.Time `json:"created_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

// Uploaded reports whether the archive is in the bucket.
func (b Backup) Uploaded() bool { return b.ObjectKey != "" }

// Restorable reports whether a copy is available.
func (b Backup) Restorable() bool {
	return b.Status == StatusOK && (b.ObjectKey != "" || b.LocalPath != "")
}

// Inventory is what the backup holds.
func (b Backup) Inventory() Inventory {
	return Inventory{Databases: b.Databases, Files: b.PlainFiles, Dirs: b.Dirs, Links: b.Links, Bytes: b.ContentBytes}
}

// Hooks are the app operations the service needs (implemented in serve).
type Hooks struct {
	// Apps lists the apps to back up on schedule.
	Apps func() []string
	// Build returns the app's current build ID ("" if none).
	Build func(app string) string
	// Reserve blocks deployments of app during a restore; it fails if a
	// deployment is in progress.
	Reserve func(app string) (unlock func(), err error)
	// Stop stops the app and reports whether it was running.
	Stop func(ctx context.Context, app string) (wasRunning bool, err error)
	// Start starts the app again.
	Start func(ctx context.Context, app string) error
	// Owner returns the app user's uid/gid.
	Owner func(app string) (uid, gid int, err error)
}

// Service runs backups and restores.
type Service struct {
	Layout layout.Layout
	Store  *store.Store
	Box    *secrets.Box
	Log    *slog.Logger
	Hooks  Hooks

	mu      sync.Mutex // one backup/restore at a time
	uploads sync.WaitGroup
}

func (s *Service) stagingDir() string { return filepath.Join(s.Layout.Root, "backups", "staging") }
func (s *Service) localDir(app string) string {
	return filepath.Join(s.Layout.Root, "backups", "local", app)
}

// Init prepares directories and marks interrupted backups.
func (s *Service) Init(ctx context.Context) error {
	for _, d := range []string{filepath.Join(s.Layout.Root, "backups"), s.stagingDir(), filepath.Join(s.Layout.Root, "backups", "local")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	entries, _ := os.ReadDir(s.stagingDir())
	for _, e := range entries {
		os.RemoveAll(filepath.Join(s.stagingDir(), e.Name()))
	}
	_, err := s.Store.Writer().ExecContext(ctx, `UPDATE backups SET status = ?, error = 'interrupted: phpboat restarted', finished_at = ?
		WHERE status = ?`, StatusFailed, time.Now().Unix(), StatusRunning)
	return err
}

// UploadPending uploads recent backups that exist only locally: uploads
// interrupted by a restart, failed uploads, and backups taken before a
// bucket was configured.
func (s *Service) UploadPending(ctx context.Context) {
	if _, ok, err := s.S3Config(ctx); err != nil || !ok {
		return
	}
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+cols+` FROM backups WHERE status = ? AND object_key = ''
		AND local_path != '' AND created_at > ? ORDER BY id`, StatusOK, time.Now().Add(-s.retention()).Unix())
	if err != nil {
		return
	}
	var todo []Backup
	for rows.Next() {
		if b, err := scan(rows); err == nil {
			todo = append(todo, b)
		}
	}
	rows.Close()
	for _, b := range todo {
		if _, err := os.Stat(b.LocalPath); err != nil {
			continue
		}
		s.mu.Lock()
		s.upload(ctx, b.ID, b.App, b.LocalPath, filepath.Base(b.LocalPath), nil)
		s.mu.Unlock()
	}
}

func (s *Service) interval() time.Duration  { return Interval }
func (s *Service) retention() time.Duration { return Retention }

// Policy returns the schedule interval and retention.
func (s *Service) Policy() (time.Duration, time.Duration) { return s.interval(), s.retention() }

// ---------------------------------------------------------------- S3 config

// S3Config returns the stored bucket config (ok=false if none).
func (s *Service) S3Config(ctx context.Context) (s3.Config, bool, error) {
	v, ok, err := s.Store.GetSetting(ctx, SettingS3)
	if err != nil || !ok {
		return s3.Config{}, false, err
	}
	b, err := s.Box.Open(v, purposeS3)
	if err != nil {
		return s3.Config{}, false, fmt.Errorf("stored S3 settings: %w", err)
	}
	var c s3.Config
	if err := json.Unmarshal(b, &c); err != nil {
		return s3.Config{}, false, err
	}
	return c, true, nil
}

// SetS3Config validates c with a test upload/download/delete and stores
// it sealed. An empty SecretKey keeps the stored secret.
func (s *Service) SetS3Config(ctx context.Context, c s3.Config) error {
	if c.SecretKey == "" {
		if old, ok, _ := s.S3Config(ctx); ok {
			c.SecretKey = old.SecretKey
		}
	}
	if err := c.Normalize(); err != nil {
		return err
	}
	cl, err := s3.New(c)
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := cl.Test(tctx); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	sealed, err := s.Box.Seal(b, purposeS3)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, SettingS3, sealed)
}

// RemoveS3Config deletes the bucket settings (backups stay local only).
func (s *Service) RemoveS3Config(ctx context.Context) error {
	_, err := s.Store.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingS3)
	return err
}

func (s *Service) client(ctx context.Context) (*s3.Client, s3.Config, bool, error) {
	c, ok, err := s.S3Config(ctx)
	if err != nil || !ok {
		return nil, c, false, err
	}
	cl, err := s3.New(c)
	return cl, c, err == nil, err
}

// Folder is where the backups of app live in the bucket: one folder per
// app at the bucket root (docs/plan.md §7).
func Folder(app string) string { return app + "/" }

// ---------------------------------------------------------------- rows

const cols = `id, app, kind, status, object_key, local_path, size, sha256, databases, plain_files, dirs, links,
	content_bytes, build_id, error, created_at, finished_at`

func scan(r interface{ Scan(...any) error }) (Backup, error) {
	var b Backup
	var c, f int64
	err := r.Scan(&b.ID, &b.App, &b.Kind, &b.Status, &b.ObjectKey, &b.LocalPath, &b.Size, &b.SHA256,
		&b.Databases, &b.PlainFiles, &b.Dirs, &b.Links, &b.ContentBytes, &b.BuildID, &b.Error, &c, &f)
	b.CreatedAt = time.Unix(c, 0)
	if f != 0 {
		b.FinishedAt = time.Unix(f, 0)
	}
	return b, err
}

// List returns an app's backups, newest first.
func (s *Service) List(ctx context.Context, app string) ([]Backup, error) {
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+cols+` FROM backups WHERE app = ? ORDER BY id DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Get returns one backup.
func (s *Service) Get(ctx context.Context, id int64) (Backup, error) {
	b, err := scan(s.Store.Reader().QueryRowContext(ctx, `SELECT `+cols+` FROM backups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return b, fmt.Errorf("backup #%d not found", id)
	}
	return b, err
}

// Latest returns the newest finished backup of app.
func (s *Service) Latest(ctx context.Context, app string) (Backup, bool) {
	b, err := scan(s.Store.Reader().QueryRowContext(ctx, `SELECT `+cols+` FROM backups WHERE app = ? AND status != ? ORDER BY id DESC LIMIT 1`, app, StatusRunning))
	return b, err == nil
}

// Problem describes why an app's latest backup needs attention ("" = fine).
// Backups whose upload is still in progress are skipped, so a problem
// stays visible until a backup actually succeeds.
func (s *Service) Problem(ctx context.Context, app string) string {
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+cols+` FROM backups WHERE app = ? AND status != ? ORDER BY id DESC LIMIT 10`, app, StatusRunning)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return ""
		}
		switch {
		case b.Status == StatusFailed:
			return "last backup failed: " + b.Error
		case b.Error == uploadPending && time.Since(b.FinishedAt) < 30*time.Minute:
			continue
		case b.Error != "":
			return "last backup is only on this server: " + b.Error
		}
		return ""
	}
	return ""
}

func (s *Service) insert(ctx context.Context, app, kind, build string) (int64, error) {
	res, err := s.Store.Writer().ExecContext(ctx, `INSERT INTO backups (app, kind, status, build_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		app, kind, StatusRunning, build, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Service) exec(q string, args ...any) {
	if _, err := s.Store.Writer().Exec(q, args...); err != nil {
		s.Log.Error("backups table", "err", err)
	}
}

// ---------------------------------------------------------------- run

// Run takes a backup of app now. The archive is kept locally; the upload
// runs in the background when wait is false (pre-deploy), so the deploy
// downtime only includes the archiving. It returns ErrEmpty (and records
// nothing) when DATA_DIR has nothing in it.
func (s *Service) Run(ctx context.Context, app, kind string, wait bool, lg *logs.Log) (Backup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run(ctx, app, kind, wait, lg)
}

func note(lg *logs.Log, format string, a ...any) {
	if lg != nil {
		lg.Writef(format, a...)
	}
}

func (s *Service) run(ctx context.Context, app, kind string, wait bool, lg *logs.Log) (Backup, error) {
	dataDir := s.Layout.DataDir(app)
	t, err := scanTree(ctx, dataDir)
	if err != nil {
		return Backup{}, err
	}
	if len(t.items) == 0 {
		return Backup{}, ErrEmpty
	}
	build := ""
	if s.Hooks.Build != nil {
		build = s.Hooks.Build(app)
	}
	id, err := s.insert(ctx, app, kind, build)
	if err != nil {
		return Backup{}, err
	}
	start := time.Now()
	fail := func(err error) (Backup, error) {
		s.exec(`UPDATE backups SET status = ?, error = ?, finished_at = ? WHERE id = ?`, StatusFailed, err.Error(), time.Now().Unix(), id)
		s.Log.Error("backup failed", "app", app, "kind", kind, "err", err)
		b, _ := s.Get(context.WithoutCancel(ctx), id)
		return b, err
	}

	if t.bytes > MaxContentBytes {
		return fail(tooLarge(t.bytes))
	}
	// The staged copy and the archive exist side by side, so the peak is about
	// twice the content.
	if err := checkFree(s.stagingDir(), 2*t.bytes+archiveSlack, "back up "+app); err != nil {
		return fail(err)
	}
	work, err := os.MkdirTemp(s.stagingDir(), app+"-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(work)
	stage := filepath.Join(work, "files")
	if err := os.Mkdir(stage, 0o700); err != nil {
		return fail(err)
	}

	// Every file is copied (or snapshotted) once into the stage, and the
	// manifest describes those copies. So each file's checksum matches the
	// bytes in the archive, even if the app writes to it during the backup.
	m := &Manifest{Version: ManifestVersion, App: app, Kind: kind, CreatedAt: start.UTC(), Build: build, PhpboatVersion: buildinfo.Version}
	// The databases are snapshotted first, then every other file is copied.
	// The manifest keeps the path order of the walk.
	order := make([]int, 0, len(t.items))
	for i, it := range t.items {
		if it.kind == KindSQLite {
			order = append(order, i)
		}
	}
	for i, it := range t.items {
		if it.kind != KindSQLite {
			order = append(order, i)
		}
	}
	staged := make([]*Entry, len(t.items))
	var content int64
	gone := 0
	for _, i := range order {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		e, err := s.stageItem(ctx, dataDir, stage, t.items[i])
		if errors.Is(err, fs.ErrNotExist) {
			gone++ // removed while the backup ran
			continue
		}
		if err != nil {
			return fail(fmt.Errorf("%s: %w", t.items[i].rel, err))
		}
		content += e.Size
		if content > MaxContentBytes {
			return fail(tooLarge(content))
		}
		staged[i] = &e
	}
	for _, e := range staged {
		if e != nil {
			m.Entries = append(m.Entries, *e)
		}
	}
	inv := inventoryOf(m)
	if esc := escapingLinks(dataDir, m); len(esc) > 0 {
		s.Log.Warn("backup: links point outside DATA_DIR", "app", app, "links", esc)
		note(lg, "%d symbolic link(s) point outside DATA_DIR (%s): they are backed up as links, but a restore does not recreate them",
			len(esc), strings.Join(esc, ", "))
	}

	name := fmt.Sprintf("%s-%s-%d.tar.zst", start.UTC().Format("20060102T150405Z"), kind, id)
	if err := os.MkdirAll(s.localDir(app), 0o700); err != nil {
		return fail(err)
	}
	local := filepath.Join(s.localDir(app), name)
	sum, size, err := writeArchive(local, m, stage)
	if err != nil {
		return fail(err)
	}
	s.exec(`UPDATE backups SET status = ?, local_path = ?, size = ?, sha256 = ?, databases = ?, plain_files = ?,
		dirs = ?, links = ?, content_bytes = ?, finished_at = ? WHERE id = ?`,
		StatusOK, local, size, sum, inv.Databases, inv.Files, inv.Dirs, inv.Links, inv.Bytes, time.Now().Unix(), id)

	extra := ""
	if t.skipped > 0 {
		extra += fmt.Sprintf(", %d special file(s) skipped", t.skipped)
	}
	if gone > 0 {
		extra += fmt.Sprintf(", %d removed during the backup", gone)
	}
	note(lg, "backup #%d: %s, %s of content, archive %s, taken in %s%s", id, inv, human(inv.Bytes), human(size),
		time.Since(start).Round(10*time.Millisecond), extra)
	s.Log.Info("backup taken", "app", app, "kind", kind, "id", id, "entries", len(m.Entries), "bytes", inv.Bytes, "archive_bytes", size)
	s.pruneLocal(app)

	if wait {
		s.upload(context.WithoutCancel(ctx), id, app, local, name, lg)
	} else {
		s.uploads.Add(1)
		go func() {
			defer s.uploads.Done()
			uctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			s.upload(uctx, id, app, local, name, nil)
		}()
	}
	return s.Get(context.WithoutCancel(ctx), id)
}

// stageItem copies one path of DATA_DIR into stage (files and SQLite
// snapshots) and describes it for the manifest. Folders and links are only
// described. A file that is not there any more returns fs.ErrNotExist.
func (s *Service) stageItem(ctx context.Context, dataDir, stage string, it item) (Entry, error) {
	e := Entry{Path: it.rel, Kind: it.kind, MTime: it.mtime}
	if it.kind != KindSymlink {
		e.Mode = modeString(it.mode)
	}
	src := filepath.Join(dataDir, filepath.FromSlash(it.rel))
	dst := filepath.Join(stage, filepath.FromSlash(it.rel))
	switch it.kind {
	case KindSymlink:
		e.Target = it.target
	case KindSQLite:
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return e, err
		}
		if err := Snapshot(ctx, src, dst); err != nil {
			return e, err
		}
		sum, n, err := fileSHA256(dst)
		if err != nil {
			return e, err
		}
		e.SHA256, e.Size = sum, n
	case KindFile:
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return e, err
		}
		sum, n, err := copyHashed(src, dst)
		if err != nil {
			return e, err
		}
		e.SHA256, e.Size = sum, n
	}
	return e, nil
}

// upload sends the archive to the bucket with retries, then applies
// retention. Without S3 settings the backup stays local only.
func (s *Service) upload(ctx context.Context, id int64, app, local, name string, lg *logs.Log) {
	cl, _, ok, err := s.client(ctx)
	if err != nil {
		s.exec(`UPDATE backups SET error = ? WHERE id = ?`, err.Error(), id)
		return
	}
	if !ok {
		note(lg, "backup #%d kept on this server only (no S3 bucket configured)", id)
		return
	}
	s.exec(`UPDATE backups SET error = ? WHERE id = ?`, uploadPending, id)
	key := Folder(app) + name
	for attempt := 1; ; attempt++ {
		err = cl.PutFile(ctx, key, local, "application/zstd")
		if err == nil || attempt == uploadAttempts || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(attempt) * uploadRetryDelay)
	}
	if err != nil {
		s.exec(`UPDATE backups SET error = ? WHERE id = ?`, err.Error(), id)
		s.Log.Error("backup upload failed", "app", app, "id", id, "err", err)
		note(lg, "backup #%d upload failed: %v", id, err)
		return
	}
	s.exec(`UPDATE backups SET object_key = ?, error = '' WHERE id = ?`, key, id)
	note(lg, "backup #%d uploaded to %s", id, key)
	s.Log.Info("backup uploaded", "app", app, "id", id, "key", key)
	if err := s.retain(ctx, cl, app, s.retention()); err != nil {
		s.Log.Warn("backup retention", "app", app, "err", err)
	}
}

// retain deletes bucket objects of app older than keep, always keeping
// the newest one, and forgets rows that no longer have any copy.
func (s *Service) retain(ctx context.Context, cl *s3.Client, app string, keep time.Duration) error {
	objs, err := cl.List(ctx, Folder(app))
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-keep)
	var errs []error
	for _, o := range Expired(objs, cutoff) {
		if err := cl.Delete(ctx, o.Key); err != nil {
			errs = append(errs, err)
			continue
		}
		s.exec(`UPDATE backups SET object_key = '' WHERE object_key = ?`, o.Key)
	}
	s.forgetEmpty(app, cutoff)
	return errors.Join(errs...)
}

// Expired returns the objects older than cutoff, never the newest one.
func Expired(objs []s3.Object, cutoff time.Time) []s3.Object {
	objs = append([]s3.Object(nil), objs...)
	sort.Slice(objs, func(i, j int) bool { return objTime(objs[i]).After(objTime(objs[j])) })
	var out []s3.Object
	for i, o := range objs {
		if i > 0 && objTime(o).Before(cutoff) {
			out = append(out, o)
		}
	}
	return out
}

// objTime is the backup time from the key name (falls back to LastModified).
func objTime(o s3.Object) time.Time {
	base := filepath.Base(o.Key)
	if len(base) >= 16 {
		if t, err := time.Parse("20060102T150405Z", base[:16]); err == nil {
			return t
		}
	}
	return o.LastModified
}

// forgetEmpty drops successful rows that have no copy left, and failed
// rows older than cutoff (they keep the failure badge visible until then).
func (s *Service) forgetEmpty(app string, cutoff time.Time) {
	s.exec(`DELETE FROM backups WHERE app = ? AND (
		(status = ? AND object_key = '' AND local_path = '') OR (status = ? AND created_at < ?))`,
		app, StatusOK, StatusFailed, cutoff.Unix())
}

// pruneLocal keeps the newest KeepLocal archives of app on disk. While a
// bucket is configured, copies that are not uploaded yet are also kept
// (until the retention ends), so an S3 outage loses nothing.
func (s *Service) pruneLocal(app string) {
	ctx := context.Background()
	_, s3ok, _ := s.S3Config(ctx)
	keep := map[string]bool{}
	rows, err := s.Store.Reader().QueryContext(ctx, `SELECT local_path, object_key, created_at FROM backups
		WHERE app = ? AND local_path != '' ORDER BY id DESC`, app)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-s.retention()).Unix()
	n := 0
	for rows.Next() {
		var path, key string
		var created int64
		if rows.Scan(&path, &key, &created) != nil {
			continue
		}
		n++
		if n <= KeepLocal || (s3ok && key == "" && created > cutoff) {
			keep[path] = true
		}
	}
	rows.Close()
	entries, _ := os.ReadDir(s.localDir(app))
	for _, e := range entries {
		p := filepath.Join(s.localDir(app), e.Name())
		if keep[p] || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		os.Remove(p)
		s.exec(`UPDATE backups SET local_path = '' WHERE local_path = ?`, p)
	}
	s.forgetEmpty(app, time.Now().Add(-s.retention()))
}

// Wait blocks until background uploads finish (shutdown, tests).
func (s *Service) Wait() { s.uploads.Wait() }

// DeleteAll removes every backup of app: bucket objects, local copies and rows.
func (s *Service) DeleteAll(ctx context.Context, app string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if cl, _, ok, err := s.client(ctx); err != nil {
		errs = append(errs, err)
	} else if ok {
		objs, err := cl.List(ctx, Folder(app))
		if err != nil {
			errs = append(errs, err)
		}
		for _, o := range objs {
			if err := cl.Delete(ctx, o.Key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if err := os.RemoveAll(s.localDir(app)); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		s.exec(`DELETE FROM backups WHERE app = ?`, app)
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------- scheduler

// nextSlot is the next multiple of every (UTC) after t.
func nextSlot(t time.Time, every time.Duration) time.Time { return t.UTC().Truncate(every).Add(every) }

// Schedule runs the 3-hourly app backups until ctx ends.
func (s *Service) Schedule(ctx context.Context) {
	go func() {
		s.UploadPending(ctx)
		for {
			next := nextSlot(time.Now(), s.interval())
			s.Log.Info("next scheduled backup", "at", next.Format(time.RFC3339))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			s.RunAll(ctx, KindScheduled)
			s.UploadPending(ctx)
		}
	}()
}

// RunAll backs up every app (one at a time).
func (s *Service) RunAll(ctx context.Context, kind string) {
	for _, app := range s.Hooks.Apps() {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.Run(ctx, app, kind, true, nil); err != nil && !errors.Is(err, ErrEmpty) {
			s.Log.Warn("scheduled backup failed", "app", app, "err", err)
		}
	}
}

// checkFree fails unless the file system of path has at least need bytes free.
func checkFree(path string, need int64, what string) error {
	free, err := freeBytes(path)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("not enough free disk space to %s: need %s, have %s", what, human(need), human(free))
	}
	return nil
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
