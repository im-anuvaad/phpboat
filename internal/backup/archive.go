package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"

	"github.com/im-anuvaad/phpboat/internal/phpfile"
)

// ManifestVersion is the archive format version. Restore refuses every other
// version. Version 1 (SQLite files only) was never released.
const ManifestVersion = 2

// Entry kinds in the manifest.
const (
	KindSQLite  = "sqlite"  // SQLite database, stored as a consistent snapshot
	KindFile    = "file"    // any other regular file, stored as it was read
	KindDir     = "dir"     // folder, kept even when it is empty
	KindSymlink = "symlink" // symbolic link, stored as a link and never followed
)

const (
	manifestName = "manifest.json"
	dataPrefix   = "data/"
	maxManifest  = 16 << 20
	maxLinkHops  = 40 // the kernel's limit for symbolic links in one path
)

// Manifest is manifest.json, the first entry of every archive. Entries list
// the whole DATA_DIR, parents before their children.
type Manifest struct {
	Version        int       `json:"version"`
	App            string    `json:"app"`
	Kind           string    `json:"kind"`
	CreatedAt      time.Time `json:"created_at"`
	Build          string    `json:"build"`
	PhpboatVersion string    `json:"phpboat_version"`
	Entries        []Entry   `json:"entries"`
}

// Entry is one path under DATA_DIR.
type Entry struct {
	Path   string    `json:"path"` // slash-separated, relative to DATA_DIR
	Kind   string    `json:"kind"`
	Size   int64     `json:"size"`             // files: bytes in the archive
	SHA256 string    `json:"sha256,omitempty"` // files: of the bytes in the archive
	Mode   string    `json:"mode,omitempty"`   // permission bits in octal, e.g. "0644"
	MTime  time.Time `json:"mtime"`
	Target string    `json:"target,omitempty"` // symlinks: the target exactly as written
}

// Inventory counts what an archive holds.
type Inventory struct {
	Databases int   // SQLite databases
	Files     int   // other regular files
	Dirs      int   // folders
	Links     int   // symbolic links
	Bytes     int64 // content of the files (SQLite snapshots included)
}

// String is a short description, e.g. "2 SQLite databases, 340 files, 12 folders".
func (c Inventory) String() string {
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(c.Databases, "SQLite database", "SQLite databases")
	add(c.Files, "file", "files")
	add(c.Dirs, "folder", "folders")
	add(c.Links, "symbolic link", "symbolic links")
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, ", ")
}

func inventoryOf(m *Manifest) Inventory {
	var c Inventory
	for _, e := range m.Entries {
		switch e.Kind {
		case KindSQLite:
			c.Databases++
			c.Bytes += e.Size
		case KindFile:
			c.Files++
			c.Bytes += e.Size
		case KindDir:
			c.Dirs++
		case KindSymlink:
			c.Links++
		}
	}
	return c
}

// MaxPHPPaths is how many PHP-looking paths a backup row keeps.
const MaxPHPPaths = 5

// phpFiles counts the files and links in m whose names look like PHP source
// (phpfile.Like) and returns the first MaxPHPPaths of their paths. phpboat
// never runs such a file (docs/architecture.md §4), but app code that includes one would.
func phpFiles(m *Manifest) (int, []string) {
	n := 0
	var paths []string
	for _, e := range m.Entries {
		if (e.Kind != KindFile && e.Kind != KindSymlink) || !phpfile.Like(e.Path) {
			continue
		}
		n++
		if len(paths) < MaxPHPPaths {
			paths = append(paths, e.Path)
		}
	}
	return n, paths
}

// writeArchive writes out: manifest.json, then every entry of m in order.
// Regular files are read from stage, a folder that holds each file at its
// entry path. The sizes and checksums in m must describe those files. It
// returns the SHA-256 and the size of the archive.
func writeArchive(out string, m *Manifest, stage string) (string, int64, error) {
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", 0, err
	}
	fh, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	zw, err := zstd.NewWriter(io.MultiWriter(fh, h), zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		fh.Close()
		os.Remove(out)
		return "", 0, err
	}
	tw := tar.NewWriter(zw)
	fail := func(err error) (string, int64, error) {
		tw.Close()
		zw.Close()
		fh.Close()
		os.Remove(out)
		return "", 0, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mj)), ModTime: m.CreatedAt, Typeflag: tar.TypeReg}); err != nil {
		return fail(err)
	}
	if _, err := tw.Write(mj); err != nil {
		return fail(err)
	}
	for _, e := range m.Entries {
		if err := writeEntry(tw, e, stage); err != nil {
			return fail(fmt.Errorf("%s: %w", e.Path, err))
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if err := fh.Sync(); err != nil {
		return fail(err)
	}
	st, err := fh.Stat()
	if err != nil {
		return fail(err)
	}
	if err := fh.Close(); err != nil {
		os.Remove(out)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), st.Size(), nil
}

func writeEntry(tw *tar.Writer, e Entry, stage string) error {
	name := dataPrefix + e.Path
	switch e.Kind {
	case KindDir:
		perm, err := permOf(e.Mode)
		if err != nil {
			return err
		}
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: int64(perm), ModTime: e.MTime})
	case KindSymlink:
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: e.Target, Mode: 0o777, ModTime: e.MTime})
	case KindFile, KindSQLite:
		perm, err := permOf(e.Mode)
		if err != nil {
			return err
		}
		src, err := os.Open(filepath.Join(stage, filepath.FromSlash(e.Path)))
		if err != nil {
			return err
		}
		defer src.Close()
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(perm), Size: e.Size, ModTime: e.MTime}); err != nil {
			return err
		}
		_, err = io.CopyN(tw, src, e.Size)
		return err
	}
	return fmt.Errorf("unknown kind %q", e.Kind)
}

// archiveReader is an open archive positioned after its manifest.
type archiveReader struct {
	f  *os.File
	zr *zstd.Decoder
	tr *tar.Reader
	m  *Manifest
}

func (a *archiveReader) Close() {
	a.zr.Close()
	a.f.Close()
}

// openArchive reads and checks the manifest, which must be the first entry.
func openArchive(path string) (*archiveReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
	if err != nil {
		f.Close()
		return nil, err
	}
	a := &archiveReader{f: f, zr: zr, tr: tar.NewReader(zr)}
	h, err := a.tr.Next()
	if err != nil || h.Name != manifestName || h.Typeflag != tar.TypeReg || h.Size > maxManifest {
		a.Close()
		return nil, errors.New("not a phpboat backup archive (manifest.json must come first)")
	}
	body, err := io.ReadAll(a.tr)
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("manifest: %w", err)
	}
	// The version is read first, so that a manifest of another format is
	// refused as such, whatever else it contains.
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		a.Close()
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if head.Version != ManifestVersion {
		a.Close()
		return nil, fmt.Errorf("unsupported backup format version %d (this phpboat reads version %d)", head.Version, ManifestVersion)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		a.Close()
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := checkManifest(&m); err != nil {
		a.Close()
		return nil, fmt.Errorf("manifest: %w", err)
	}
	a.m = &m
	return a, nil
}

// readManifest returns the manifest of an archive after checking it. No
// file content is read.
func readManifest(path string) (*Manifest, error) {
	a, err := openArchive(path)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	return a.m, nil
}

// checkManifest refuses manifests that could not come from a valid backup
// or that would write outside DATA_DIR.
func checkManifest(m *Manifest) error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("unsupported backup format version %d (this phpboat reads version %d)", m.Version, ManifestVersion)
	}
	seen := make(map[string]bool, len(m.Entries))
	var total int64
	for _, e := range m.Entries {
		if !validRel(e.Path) {
			return fmt.Errorf("unsafe path %q", e.Path)
		}
		if seen[e.Path] {
			return fmt.Errorf("%s is listed twice", e.Path)
		}
		seen[e.Path] = true
		if strings.HasPrefix(strings.SplitN(e.Path, "/", 2)[0], preRestorePrefix) {
			return fmt.Errorf("%s uses a name that phpboat reserves", e.Path)
		}
		switch e.Kind {
		case KindFile, KindSQLite:
			if e.Size < 0 || !validSHA(e.SHA256) {
				return fmt.Errorf("%s: bad size or checksum", e.Path)
			}
			if _, err := permOf(e.Mode); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			total += e.Size
			if total > MaxContentBytes {
				return fmt.Errorf("archive content is larger than the limit of %s (%s)", gigabytes(MaxContentBytes), gigabytes(total))
			}
		case KindDir:
			if _, err := permOf(e.Mode); err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
		case KindSymlink:
			if e.Target == "" || !utf8.ValidString(e.Target) || strings.ContainsRune(e.Target, 0) {
				return fmt.Errorf("%s: bad link target %q", e.Path, e.Target)
			}
		default:
			return fmt.Errorf("%s: unknown kind %q", e.Path, e.Kind)
		}
	}
	return nil
}

// extractArchive unpacks the archive into dir (an empty folder) and checks
// every entry against the manifest: names, types, sizes, SHA-256 sums. A path
// that would lead outside dir is refused before anything is written there.
//
// Symbolic links whose target leads outside dir are not restored: links are
// not data, phpboat never follows them, and recreating them would let a
// restored app reach files outside its DATA_DIR. Their paths are returned,
// and the returned manifest no longer lists them.
func extractArchive(archive, dir string) (*Manifest, []string, error) {
	a, err := openArchive(archive)
	if err != nil {
		return nil, nil, err
	}
	defer a.Close()
	m := a.m
	byName := make(map[string]*Entry, len(m.Entries))
	for i := range m.Entries {
		byName[dataPrefix+m.Entries[i].Path] = &m.Entries[i]
	}
	seen := make(map[string]bool, len(m.Entries))
	var dirs, links []*Entry
	for {
		h, err := a.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("archive: %w", err)
		}
		e := byName[h.Name]
		if e == nil || seen[h.Name] {
			return nil, nil, fmt.Errorf("unexpected entry %q in archive", h.Name)
		}
		seen[h.Name] = true
		switch e.Kind {
		case KindDir:
			if h.Typeflag != tar.TypeDir || h.Size != 0 {
				return nil, nil, fmt.Errorf("%s: not a folder in the archive", e.Path)
			}
			if err := mkdirUnder(dir, e.Path); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			dirs = append(dirs, e)
		case KindSymlink:
			if h.Typeflag != tar.TypeSymlink || h.Linkname != e.Target {
				return nil, nil, fmt.Errorf("%s: the link in the archive differs from the manifest", e.Path)
			}
			if err := mkdirUnder(dir, filepath.Dir(filepath.FromSlash(e.Path))); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			if err := os.Symlink(e.Target, filepath.Join(dir, filepath.FromSlash(e.Path))); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.Path, err)
			}
			links = append(links, e)
		default:
			if h.Typeflag != tar.TypeReg || h.Size != e.Size {
				return nil, nil, fmt.Errorf("%s: size or type differs from the manifest", e.Path)
			}
			if err := writeStaged(dir, e, a.tr); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", e.Path, err)
			}
		}
	}
	for _, e := range m.Entries {
		if !seen[dataPrefix+e.Path] {
			return nil, nil, fmt.Errorf("archive is missing %s, which its manifest lists", e.Path)
		}
	}
	skipped, err := dropEscapingLinks(dir, links)
	if err != nil {
		return nil, nil, err
	}
	if len(skipped) > 0 {
		m.Entries = withoutPaths(m.Entries, skipped)
	}
	// Folders get their modes and times last, deepest first, so that
	// read-only folders do not stop their own contents from being written.
	sort.SliceStable(dirs, func(i, j int) bool { return len(dirs[i].Path) > len(dirs[j].Path) })
	for _, d := range dirs {
		perm, err := permOf(d.Mode)
		if err != nil {
			return nil, nil, err
		}
		p := filepath.Join(dir, filepath.FromSlash(d.Path))
		if err := os.Chmod(p, perm|0o700); err != nil {
			return nil, nil, err
		}
		if err := os.Chtimes(p, d.MTime, d.MTime); err != nil {
			return nil, nil, err
		}
	}
	return m, skipped, nil
}

// dropEscapingLinks removes from dir the links that can lead outside it and
// returns their paths. Removing one link can change where another resolves,
// so the check repeats until no link fails.
func dropEscapingLinks(dir string, links []*Entry) ([]string, error) {
	var skipped []string
	for {
		var bad, keep []*Entry
		for _, e := range links {
			if checkLink(dir, e.Path, e.Target) != nil {
				bad = append(bad, e)
			} else {
				keep = append(keep, e)
			}
		}
		if len(bad) == 0 {
			return skipped, nil
		}
		for _, e := range bad {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(e.Path))); err != nil {
				return nil, err
			}
			skipped = append(skipped, e.Path)
		}
		links = keep
	}
}

// withoutPaths returns the entries whose path is not in drop.
func withoutPaths(entries []Entry, drop []string) []Entry {
	gone := make(map[string]bool, len(drop))
	for _, p := range drop {
		gone[p] = true
	}
	var out []Entry
	for _, e := range entries {
		if !gone[e.Path] {
			out = append(out, e)
		}
	}
	return out
}

// escapingLinks returns the symbolic links of m that lead outside root, as
// they are now in root. A backup reports them: they are kept, but a restore
// does not recreate them.
func escapingLinks(root string, m *Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		if e.Kind == KindSymlink && checkLink(root, e.Path, e.Target) != nil {
			out = append(out, e.Path)
		}
	}
	return out
}

// writeStaged creates a regular file of e under dir and copies exactly its
// size from r. The file is created exclusively, under folders that are
// checked not to be links.
func writeStaged(dir string, e *Entry, r io.Reader) error {
	if err := mkdirUnder(dir, filepath.Dir(filepath.FromSlash(e.Path))); err != nil {
		return err
	}
	perm, err := permOf(e.Mode)
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.FromSlash(e.Path))
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.CopyN(io.MultiWriter(f, h), r, e.Size)
	if err == nil {
		err = f.Chmod(perm | 0o600)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return errors.New("checksum does not match the manifest")
	}
	return os.Chtimes(dst, e.MTime, e.MTime)
}

// validRel reports whether p is a clean relative path that stays inside its
// root.
func validRel(p string) bool {
	return p != "" && utf8.ValidString(p) && !strings.ContainsRune(p, 0) &&
		filepath.IsLocal(p) && filepath.Clean(p) == p
}

func validSHA(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// permOf parses an octal mode from the manifest. Setuid, setgid and sticky
// bits are dropped: restored files never carry them.
func permOf(s string) (fs.FileMode, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o7777 {
		return 0, fmt.Errorf("bad mode %q", s)
	}
	return fs.FileMode(v) & 0o777, nil
}

func modeString(m fs.FileMode) string { return fmt.Sprintf("%04o", m.Perm()) }
