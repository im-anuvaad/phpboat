package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/im-anuvaad/phpboat/internal/logs"
)

// KeepPreRestore is how many .pre-restore-* folders are kept per app.
const KeepPreRestore = 2

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Counts       Inventory // what was put back
	SkippedLinks []string  // links that pointed outside DATA_DIR and were not put back
	MovedTo      string    // folder with the data that the restore replaced ("" if there was none)
	WasRunning   bool      // the app was running before the restore
	Restarted    bool
	StartError   string
}

// Restore replaces the whole DATA_DIR of app with backup id.
//
// The archive is fetched and verified in staging while the app keeps running:
// manifest, every checksum and size, every database (quick_check), every
// link target. Only after the free-space check does the app stop. The current
// DATA_DIR content then moves to DATA_DIR/.pre-restore-<unix-time>/, the
// verified content moves in and goes to the app user, and the app starts
// again if it was running. A failed move is undone, so the data is back as it
// was.
func (s *Service) Restore(ctx context.Context, app string, id int64, lg *logs.Log) (RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res RestoreResult
	b, err := s.Get(ctx, id)
	if err != nil {
		return res, err
	}
	if b.App != app {
		return res, fmt.Errorf("backup #%d belongs to %s, not %s", id, b.App, app)
	}
	if !b.Restorable() {
		return res, fmt.Errorf("backup #%d cannot be restored (status %s, no copy available)", id, b.Status)
	}
	unlock, err := s.Hooks.Reserve(app)
	if err != nil {
		return res, err
	}
	defer unlock()

	work, err := os.MkdirTemp(s.stagingDir(), "restore-"+app+"-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(work)

	// 1. Get the archive and read its manifest. The app keeps running.
	archive, err := s.fetch(ctx, b, work)
	if err != nil {
		return res, err
	}
	m, err := readManifest(archive)
	if err != nil {
		return res, fmt.Errorf("backup #%d failed verification: %w", id, err)
	}
	if m.App != app {
		return res, fmt.Errorf("backup #%d contains %s, not %s", id, m.App, app)
	}
	dataDir := s.Layout.DataDir(app)
	cur, err := contentSize(dataDir)
	if err != nil {
		return res, err
	}
	inv := inventoryOf(m)
	// Both must fit: the restored content is extracted while the current
	// content is still in place, and the current content stays on disk as the
	// .pre-restore copy.
	if err := checkFree(dataDir, inv.Bytes+cur, "restore"); err != nil {
		return res, err
	}

	// 2. Verify everything in staging, still with the app running.
	uid, gid, err := s.Hooks.Owner(app)
	if err != nil {
		return res, err
	}
	out := filepath.Join(work, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return res, err
	}
	staged, skipped, err := s.stageVerified(ctx, archive, out, uid, gid)
	if err != nil {
		return res, fmt.Errorf("backup #%d failed verification: %w", id, err)
	}
	if err := sameDevice(out, dataDir); err != nil {
		return res, err
	}
	res.Counts = inventoryOf(staged)
	res.SkippedLinks = skipped
	note(lg, "backup #%d verified: %s, %s of content", id, res.Counts, human(res.Counts.Bytes))

	// 3. Swap the data while the app is stopped.
	wasRunning, err := s.Hooks.Stop(ctx, app)
	if err != nil {
		return res, fmt.Errorf("stop %s: %w", app, err)
	}
	res.WasRunning = wasRunning
	mv := &mover{}
	pre, err := s.moveAside(dataDir, mv)
	if err == nil {
		err = placeStaged(out, dataDir, mv)
	}
	if err != nil {
		if uerr := mv.undo(); uerr != nil {
			err = fmt.Errorf("%w; putting the data back also failed: %v", err, uerr)
		}
		if pre != "" {
			os.Remove(pre) // only if the undo left it empty
		}
		s.restart(ctx, app, &res)
		return res, err
	}
	res.MovedTo = pre
	if pre != "" {
		note(lg, "previous data moved to %s", pre)
	}
	s.prunePreRestore(dataDir)
	note(lg, "restored %s", res.Counts)
	s.Log.Info("backup restored", "app", app, "id", id, "entries", len(staged.Entries), "moved_to", pre, "skipped_links", len(skipped))
	s.restart(ctx, app, &res)
	return res, nil
}

// restart starts the app again if it was running before a restore.
func (s *Service) restart(ctx context.Context, app string, res *RestoreResult) {
	if !res.WasRunning {
		return
	}
	if err := s.Hooks.Start(context.WithoutCancel(ctx), app); err != nil {
		res.StartError = err.Error()
		return
	}
	res.Restarted = true
}

// stageVerified extracts archive into out, which must be empty. It checks
// each database with quick_check, drops the files SQLite created while
// checking, and gives the tree to uid:gid. It returns the manifest of what
// is staged and the links that were not restored.
func (s *Service) stageVerified(ctx context.Context, archive, out string, uid, gid int) (*Manifest, []string, error) {
	m, skipped, err := extractArchive(archive, out)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range m.Entries {
		if e.Kind != KindSQLite {
			continue
		}
		if err := QuickCheck(ctx, filepath.Join(out, filepath.FromSlash(e.Path))); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", e.Path, err)
		}
	}
	if err := pruneStray(out, m); err != nil {
		return nil, nil, err
	}
	if err := chownTree(out, uid, gid); err != nil {
		return nil, nil, err
	}
	return m, skipped, nil
}

// fetch returns a verified local archive: the local copy if it is intact,
// otherwise a download from the bucket into work.
func (s *Service) fetch(ctx context.Context, b Backup, work string) (string, error) {
	if b.LocalPath != "" {
		if sum, _, err := fileSHA256(b.LocalPath); err == nil && sum == b.SHA256 {
			return b.LocalPath, nil
		}
	}
	if b.ObjectKey == "" {
		return "", fmt.Errorf("backup #%d: the local copy is missing or damaged and it was never uploaded", b.ID)
	}
	cl, _, ok, err := s.client(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("the local copy is gone and no S3 bucket is configured")
	}
	dst := filepath.Join(work, "download.tar.zst")
	if err := cl.GetFile(ctx, b.ObjectKey, dst); err != nil {
		return "", err
	}
	sum, _, err := fileSHA256(dst)
	if err != nil {
		return "", err
	}
	if b.SHA256 != "" && sum != b.SHA256 {
		return "", fmt.Errorf("backup #%d: downloaded archive checksum does not match (expected %s, got %s)", b.ID, b.SHA256, sum)
	}
	return dst, nil
}

// moveAside moves every entry of dataDir except the .pre-restore-* folders
// into a new .pre-restore-<unix-time> folder and returns its path ("" when
// there was nothing to move).
func (s *Service) moveAside(dataDir string, mv *mover) (string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !isPreRestore(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	pre, err := newPreRestoreDir(dataDir)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if err := mv.move(filepath.Join(dataDir, n), filepath.Join(pre, n)); err != nil {
			return pre, err
		}
	}
	return pre, nil
}

// newPreRestoreDir creates .pre-restore-<unix-time> in dataDir. A second
// restore in the same second gets a -2, -3 suffix, so no safety copy is
// overwritten.
func newPreRestoreDir(dataDir string) (string, error) {
	base := preRestorePrefix + strconv.FormatInt(time.Now().Unix(), 10)
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		p := filepath.Join(dataDir, name)
		err := os.Mkdir(p, 0o700)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
}

// placeStaged moves every top-level entry of the verified tree out into
// dataDir.
func placeStaged(out, dataDir string, mv *mover) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := mv.move(filepath.Join(out, e.Name()), filepath.Join(dataDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// prunePreRestore keeps the KeepPreRestore newest .pre-restore-* folders of
// dataDir and deletes the others. Only names this code creates are touched.
func (s *Service) prunePreRestore(dataDir string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	type preDir struct {
		name string
		at   int64
		n    int64
	}
	var list []preDir
	for _, e := range entries {
		if !isPreRestore(e.Name()) {
			continue
		}
		if at, n, ok := parsePreRestore(e.Name()); ok {
			list = append(list, preDir{e.Name(), at, n})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].at != list[j].at {
			return list[i].at > list[j].at
		}
		return list[i].n > list[j].n
	})
	for _, p := range list[min(len(list), KeepPreRestore):] {
		if err := os.RemoveAll(filepath.Join(dataDir, p.name)); err != nil {
			s.Log.Warn("removing an old pre-restore folder", "path", p.name, "err", err)
		}
	}
}

// parsePreRestore reads the time (and the same-second counter) from a
// .pre-restore-<unix-time>[-n] name.
func parsePreRestore(name string) (at, n int64, ok bool) {
	a, b, hasN := strings.Cut(strings.TrimPrefix(name, preRestorePrefix), "-")
	at, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if hasN {
		if n, err = strconv.ParseInt(b, 10, 64); err != nil {
			return 0, 0, false
		}
	}
	return at, n, true
}

// mover renames paths and remembers every rename, so that a failed restore
// can undo them in reverse order.
type mover struct{ done [][2]string }

func (m *mover) move(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	m.done = append(m.done, [2]string{from, to})
	return nil
}

func (m *mover) undo() error {
	var errs []error
	for i := len(m.done) - 1; i >= 0; i-- {
		if err := os.Rename(m.done[i][1], m.done[i][0]); err != nil {
			errs = append(errs, err)
		}
	}
	m.done = nil
	return errors.Join(errs...)
}
