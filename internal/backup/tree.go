package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

var sqliteHeader = []byte("SQLite format 3\x00")

// sidecarSuffixes are SQLite's companion files. The snapshot of the database
// already holds their content, so they are never copied on their own.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

// item is one path that a backup includes. It keeps only what the manifest
// needs: a backup of a folder with many files holds one item per file.
type item struct {
	rel    string // slash-separated, relative to DATA_DIR
	kind   string
	mode   fs.FileMode
	size   int64
	mtime  time.Time
	target string // symbolic links
}

// tree is what a backup of DATA_DIR includes, before anything is copied.
type tree struct {
	items   []item
	skipped int   // special files: sockets, FIFOs, devices
	bytes   int64 // size of the regular files (a database by its file size)
}

// preRestorePrefix names the top-level entries of DATA_DIR that hold the data
// a restore replaced. They are phpboat's: never backed up, never restored,
// never moved aside by a restore.
const preRestorePrefix = ".pre-restore-"

// isPreRestore reports whether a top-level name of DATA_DIR is reserved.
func isPreRestore(name string) bool { return strings.HasPrefix(name, preRestorePrefix) }

// scanTree lists what a backup of dataDir includes: folders, links, regular
// files and SQLite databases (by their header, not their name). Special
// files are counted and skipped. A missing dataDir is an empty tree.
func scanTree(ctx context.Context, dataDir string) (tree, error) {
	var t tree
	var raw []item
	err := filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dataDir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while the backup walks the tree
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == dataDir {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filepath.Dir(rel) == "." && isPreRestore(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		typ := d.Type()
		if !typ.IsDir() && !typ.IsRegular() && typ&fs.ModeSymlink == 0 {
			t.skipped++
			return nil
		}
		if !utf8.ValidString(rel) {
			return fmt.Errorf("%q: the name is not valid UTF-8, so it cannot be backed up; rename it", rel)
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		it := item{rel: rel, mode: info.Mode(), size: info.Size(), mtime: info.ModTime().UTC()}
		switch {
		case typ.IsDir():
			it.kind = KindDir
			raw = append(raw, it)
		case typ&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !utf8.ValidString(target) {
				return fmt.Errorf("%s: the link target is not valid UTF-8, so the link cannot be backed up", rel)
			}
			it.kind, it.target = KindSymlink, target
			raw = append(raw, it)
		default:
			isDB, err := isSQLite(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			it.kind = KindFile
			if isDB {
				it.kind = KindSQLite
			}
			raw = append(raw, it)
		}
		return nil
	})
	if err != nil {
		return t, err
	}
	dbs := make(map[string]bool)
	for _, it := range raw {
		if it.kind == KindSQLite {
			dbs[it.rel] = true
		}
	}
	for _, it := range raw {
		if it.kind == KindFile && isSidecar(it.rel, dbs) {
			continue
		}
		t.items = append(t.items, it)
		if it.kind == KindFile || it.kind == KindSQLite {
			t.bytes += it.size
		}
	}
	return t, nil
}

// isSidecar reports whether rel is the -wal, -shm or -journal file of a
// database in dbs.
func isSidecar(rel string, dbs map[string]bool) bool {
	for _, s := range sidecarSuffixes {
		if strings.HasSuffix(rel, s) && dbs[strings.TrimSuffix(rel, s)] {
			return true
		}
	}
	return false
}

// isSQLite reports whether the regular file at path starts with SQLite's
// header. It opens the file without blocking, so a FIFO that replaces the
// file cannot hang the caller.
func isSQLite(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, nil
	}
	hdr := make([]byte, len(sqliteHeader))
	n, err := io.ReadFull(f, hdr)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(hdr) && bytes.Equal(hdr, sqliteHeader), nil
}

// contentSize is the total size of the regular files in dataDir, leaving out
// the .pre-restore-* folders.
func contentSize(dataDir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == dataDir {
			return nil
		}
		if rel, _ := filepath.Rel(dataDir, p); filepath.Dir(rel) == "." && isPreRestore(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	return total, err
}
