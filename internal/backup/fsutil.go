package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// mkdirUnder creates the folders of rel under root that do not exist. Every
// component that exists must be a real folder: a link or a file there
// fails, so nothing is ever written through a link.
func mkdirUnder(root, rel string) error {
	cur := root
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
		case err == nil:
			return fmt.Errorf("%s is in the way (not a folder)", seg)
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}

// checkLink fails if the symbolic link at rel (under root) with the given
// target can lead outside root. The target is followed one component at a
// time, as the kernel does, so a link to a link is caught as well. Missing
// names are walked as plain names, which is the stricter reading.
func checkLink(root, rel, target string) error {
	if filepath.IsAbs(target) {
		return errors.New("points outside DATA_DIR")
	}
	cur := splitPath(filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))))
	queue := splitPath(target)
	hops := 0
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		switch c {
		case "..":
			if len(cur) == 0 {
				return errors.New("points outside DATA_DIR")
			}
			cur = cur[:len(cur)-1]
			continue
		}
		p := filepath.Join(append(append([]string{root}, cur...), c)...)
		fi, err := os.Lstat(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			if hops++; hops > maxLinkHops {
				return errors.New("too many levels of links")
			}
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if filepath.IsAbs(t) {
				return errors.New("points outside DATA_DIR")
			}
			queue = append(splitPath(t), queue...)
			continue
		}
		cur = append(cur, c)
	}
	return nil
}

// splitPath returns the names of a slash-separated path, without "" and ".".
func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(filepath.ToSlash(p), "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// freeBytes is the space available to unprivileged writers on the file system of path.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// sameDevice fails unless a and b are on the same file system, which a
// rename between them needs.
func sameDevice(a, b string) error {
	sa, err := os.Stat(a)
	if err != nil {
		return err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return err
	}
	ta, ok1 := sa.Sys().(*syscall.Stat_t)
	tb, ok2 := sb.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return errors.New("cannot compare file systems on this platform")
	}
	if ta.Dev != tb.Dev {
		return fmt.Errorf("%s and %s are on different file systems", a, b)
	}
	return nil
}

// chownTree gives every entry under dir (links included) to uid:gid.
func chownTree(dir string, uid, gid int) error {
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// copyHashed copies the regular file src to a new file dst and returns the
// SHA-256 and the size of what it read. The source is opened without
// blocking, so a file replaced by a FIFO cannot hang the backup.
func copyHashed(src, dst string) (string, int64, error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, fs.ErrNotExist // no longer a regular file: treated as gone
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// fileSHA256 hashes a file and returns its size too.
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// pruneStray removes the regular files under dir that are not in m, such as
// the -wal and -shm files SQLite creates when it opens a database.
func pruneStray(dir string, m *Manifest) error {
	want := make(map[string]bool, len(m.Entries))
	for _, e := range m.Entries {
		want[filepath.FromSlash(e.Path)] = true
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if !want[rel] {
			return os.Remove(p)
		}
		return nil
	})
}

// tooLarge is the backup error for a data folder above MaxContentBytes.
func tooLarge(n int64) error {
	return fmt.Errorf("data folder too large to back up (%s)", gigabytes(n))
}

// gigabytes rounds n to a tenth of a gigabyte. A value above the limit never
// reads as the limit itself: it shows at least one tenth more.
func gigabytes(n int64) string {
	limit := int64(MaxContentBytes * 10 / (1 << 30)) // the limit in tenths
	t := (n*10 + 1<<29) / (1 << 30)
	if n > MaxContentBytes && t <= limit {
		t = limit + 1
	}
	return fmt.Sprintf("%d.%d GB", t/10, t%10)
}
