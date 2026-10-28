// Package artifact unpacks the source tarball of a commit and hashes it.
// Extraction is strict: anything that could write outside the destination
// is refused.
package artifact

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxUnpacked bounds the unpacked size of a source tree.
const MaxUnpacked = 1 << 30

// MaxEntries bounds the number of entries in a source tarball.
const MaxEntries = 20000

// FileSHA256 hashes a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Unpack extracts the gzipped tarball archive into dst (created). It
// rejects absolute paths, "..", device files, hard links, symlinks that
// point outside dst, paths through symlinks, and archives that expand to
// more than max bytes. Files get mode 0644, or 0755 if any exec bit is set.
func Unpack(archive, dst string, max int64) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip file: %w", err)
	}
	defer zr.Close()
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for n := 0; ; n++ {
		if n > MaxEntries {
			return fmt.Errorf("archive has more than %d entries", MaxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || name == "." || name == "./" {
			continue
		}
		rel := filepath.Clean(name)
		if filepath.IsAbs(name) || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		target := filepath.Join(dst, rel)
		// A path through a symlink extracted earlier could escape dst
		// (a/b -> .., a/b/c -> .. resolves outside), so parents must be
		// real directories and files must not exist yet.
		if err := realParents(dst, rel); err != nil {
			return fmt.Errorf("unsafe path %q in archive: %w", h.Name, err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if st, err := os.Lstat(target); err == nil && !st.IsDir() {
				return fmt.Errorf("unsafe path %q in archive: exists and is not a directory", h.Name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > max {
				return fmt.Errorf("archive expands to more than %d MB", max>>20)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(out, tr, h.Size); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			lt := h.Linkname
			resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), lt))
			if filepath.IsAbs(lt) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink %q -> %q escapes the archive", h.Name, lt)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(lt, target); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// metadata only
		default:
			return fmt.Errorf("unsupported entry type %q for %q", h.Typeflag, h.Name)
		}
	}
}

// realParents checks that every existing parent of rel below dst is a
// directory, not a symlink.
func realParents(dst, rel string) error {
	cur := dst
	for _, p := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if p == "." || p == "" {
			continue
		}
		cur = filepath.Join(cur, p)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", strings.TrimPrefix(cur, dst+"/"))
		}
	}
	return nil
}
