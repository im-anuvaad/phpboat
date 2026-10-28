package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/im-anuvaad/phpboat/internal/s3"
)

// Folders lists the backup folders in the bucket (one per app name), for
// the Add app form. ok is false without a bucket.
func (s *Service) Folders(ctx context.Context) (folders []string, ok bool, err error) {
	cl, _, ok, err := s.client(ctx)
	if err != nil || !ok {
		return nil, ok, err
	}
	all, err := cl.Folders(ctx)
	if err != nil {
		return nil, true, err
	}
	for _, f := range all {
		if !strings.HasPrefix(f, ".") {
			folders = append(folders, f)
		}
	}
	return folders, true, nil
}

// RestoreFolder puts the newest backup in a bucket folder into the empty
// DATA_DIR of a new app (a new server, or an app that was deleted). The
// archive is verified completely in staging before anything is moved into
// DATA_DIR. The folder may have another name than the app (a renamed repo),
// but the archive must have been made under the folder's name. It returns
// the top-level names that were restored.
func (s *Service) RestoreFolder(ctx context.Context, app, folder string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cl, _, ok, err := s.client(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("no bucket is configured")
	}
	if strings.ContainsAny(folder, "/\\") || folder == "" || strings.HasPrefix(folder, ".") {
		return nil, fmt.Errorf("invalid backup folder %q", folder)
	}
	objs, err := cl.List(ctx, Folder(folder))
	if err != nil {
		return nil, err
	}
	o, ok := Newest(objs)
	if !ok {
		return nil, fmt.Errorf("the folder %s/ has no backups", folder)
	}
	dataDir := s.Layout.DataDir(app)
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return nil, fmt.Errorf("%s already has data in DATA_DIR; restore from the app page instead", app)
	}
	uid, gid, err := s.Hooks.Owner(app)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(s.stagingDir(), "folder-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	archive := filepath.Join(work, "download.tar.zst")
	if err := cl.GetFile(ctx, o.Key, archive); err != nil {
		return nil, err
	}
	m, err := readManifest(archive)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Key, err)
	}
	if m.App != folder {
		return nil, fmt.Errorf("%s holds backups of %s, not %s", o.Key, m.App, folder)
	}
	inv := inventoryOf(m)
	if err := checkFree(dataDir, inv.Bytes, "restore"); err != nil {
		return nil, err
	}
	out := filepath.Join(work, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		return nil, err
	}
	staged, skipped, err := s.stageVerified(ctx, archive, out, uid, gid)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Key, err)
	}
	if err := sameDevice(out, dataDir); err != nil {
		return nil, err
	}
	mv := &mover{}
	if err := placeStaged(out, dataDir, mv); err != nil {
		if uerr := mv.undo(); uerr != nil {
			err = fmt.Errorf("%w; undoing it also failed: %v", err, uerr)
		}
		return nil, err
	}
	var names []string
	for _, e := range staged.Entries {
		if !strings.Contains(e.Path, "/") {
			names = append(names, e.Path)
		}
	}
	sort.Strings(names)
	s.Log.Info("restored a bucket folder into a new app", "app", app, "folder", folder, "key", o.Key,
		"content", inventoryOf(staged).String(), "skipped_links", skipped)
	return names, nil
}

// Newest returns the newest backup object by the time in its name, skipping
// anything that is not a phpboat archive.
func Newest(objs []s3.Object) (s3.Object, bool) {
	var ok []s3.Object
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".tar.zst") {
			ok = append(ok, o)
		}
	}
	if len(ok) == 0 {
		return s3.Object{}, false
	}
	sort.Slice(ok, func(i, j int) bool { return objTime(ok[i]).After(objTime(ok[j])) })
	return ok[0], true
}
