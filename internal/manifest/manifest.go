// Package manifest reads an app's optional phpboat.toml
// (docs/app-contract.md §3) and validates it against the repository it
// comes with.
package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/php"
)

// FileName is the manifest file name at the app root.
const FileName = "phpboat.toml"

// DefaultDocroot is the public folder when phpboat.toml doesn't say.
const DefaultDocroot = "public"

const (
	maxSize        = 64 << 10
	maxPersist     = 20
	maxBeforeStart = 10
)

// Manifest is a validated phpboat.toml (or the defaults without one). It is
// stored with each build, so later starts use what the build was made with.
type Manifest struct {
	Contract    int          `json:"contract"`
	Docroot     string       `json:"docroot"` // "public", "." or another folder, relative to the repository root
	HealthPath  string       `json:"health_path"`
	Persist     []string     `json:"persist,omitempty"`      // folders kept in DATA_DIR across deploys
	BeforeStart []string     `json:"before_start,omitempty"` // PHP scripts run before every start
	PHP         php.Settings `json:"php,omitempty"`
}

// Default is the manifest of a repo without phpboat.toml.
func Default() Manifest {
	return Manifest{Contract: app.ContractVersion, Docroot: DefaultDocroot, HealthPath: app.DefaultHealthPath}
}

type raw struct {
	Contract    *int           `toml:"contract"`
	Docroot     *string        `toml:"docroot"`
	HealthPath  *string        `toml:"health_path"`
	Persist     []string       `toml:"persist"`
	BeforeStart []string       `toml:"before_start"`
	PHP         map[string]any `toml:"php"`
}

// later lists contract keys this phpboat does not implement yet, so they
// fail with a clear message instead of "unknown key".
var later = map[string]bool{"cron": true}

// Load reads dir/phpboat.toml (defaults if there is none) and checks the
// repository against it: the docroot, persist folders and before_start
// scripts must exist as the manifest describes.
func Load(dir string) (Manifest, error) {
	m := Default()
	p := filepath.Join(dir, FileName)
	f, err := os.Open(p)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return m, err
	default:
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			return m, fmt.Errorf("%s must be a regular file", FileName)
		}
		b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
		if err != nil {
			return m, err
		}
		if len(b) > maxSize {
			return m, fmt.Errorf("%s is larger than 64 KB", FileName)
		}
		if m, err = Parse(b); err != nil {
			return m, err
		}
	}
	if err := checkDocroot(dir, m.Docroot); err != nil {
		return m, err
	}
	return m, checkTree(dir, m)
}

func checkDocroot(dir, docroot string) error {
	p := filepath.Join(dir, docroot)
	st, err := os.Lstat(p)
	if err != nil || !st.IsDir() {
		if docroot == DefaultDocroot {
			return fmt.Errorf("the repository has no %s/ folder: put the public files (index.php, css, images) in %s/, or set docroot in %s (docroot = \".\" for the repository root)", DefaultDocroot, DefaultDocroot, FileName)
		}
		return fmt.Errorf("docroot %q is not a folder in the repository", docroot)
	}
	return nil
}

// checkTree checks persist folders and before_start scripts against the
// repository. Nothing may go through a symbolic link: the build would then
// point outside itself.
func checkTree(dir string, m Manifest) error {
	var errs []error
	for _, p := range m.Persist {
		fi, err := lstatUnder(dir, p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Created empty in DATA_DIR on the first deploy.
		case err != nil || !fi.IsDir():
			errs = append(errs, fmt.Errorf("persist folder %q must be a folder in the repository (or missing) and not a symbolic link", p))
		}
	}
	for _, b := range m.BeforeStart {
		fi, err := lstatUnder(dir, b)
		if err != nil || !fi.Mode().IsRegular() {
			errs = append(errs, fmt.Errorf("before_start script %q is not a file in the repository", b))
		}
	}
	return errors.Join(errs...)
}

// lstatUnder stats rel below dir without following symbolic links.
func lstatUnder(dir, rel string) (os.FileInfo, error) {
	cur := dir
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		cur = filepath.Join(cur, s)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if i == len(segs)-1 {
			return fi, nil
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("%q is not a folder", rel)
		}
	}
	return nil, fs.ErrNotExist
}

// Parse validates manifest content, reporting every problem at once.
func Parse(b []byte) (Manifest, error) {
	var r raw
	m := Default()
	md, err := toml.Decode(string(b), &r)
	if err != nil {
		return m, fmt.Errorf("%s: %w", FileName, err)
	}
	var errs []error
	seen := map[string]bool{}
	for _, k := range md.Undecoded() {
		top := k[0]
		if seen[top] {
			continue
		}
		seen[top] = true
		if later[top] {
			errs = append(errs, fmt.Errorf("%q is not supported by this phpboat version yet", top))
		} else {
			errs = append(errs, fmt.Errorf("unknown key %q", k.String()))
		}
	}
	switch {
	case r.Contract == nil:
		errs = append(errs, fmt.Errorf("contract is required (use contract = %d)", app.ContractVersion))
	case *r.Contract != app.ContractVersion:
		errs = append(errs, fmt.Errorf("contract = %d is not supported by this phpboat (supported: %d)", *r.Contract, app.ContractVersion))
	}
	if r.Docroot != nil {
		d, err := cleanDocroot(*r.Docroot)
		if err != nil {
			errs = append(errs, err)
		} else {
			m.Docroot = d
		}
	}
	if r.HealthPath != nil {
		if !strings.HasPrefix(*r.HealthPath, "/") || strings.ContainsAny(*r.HealthPath, " \t\r\n") {
			errs = append(errs, fmt.Errorf("health_path %q must start with '/' and contain no spaces", *r.HealthPath))
		} else {
			m.HealthPath = *r.HealthPath
		}
	}
	persist, perrs := cleanPersist(r.Persist, m.Docroot)
	errs = append(errs, perrs...)
	m.Persist = persist
	before, berrs := cleanBeforeStart(r.BeforeStart, persist)
	errs = append(errs, berrs...)
	m.BeforeStart = before
	if len(r.PHP) > 0 {
		set := php.Settings{}
		for k, v := range r.PHP {
			switch v := v.(type) {
			case string:
				set[k] = v
			case int64:
				set[k] = strconv.FormatInt(v, 10)
			default:
				errs = append(errs, fmt.Errorf("[php] %s must be a string or a whole number", k))
			}
		}
		if err := php.ValidateSettings(set); err != nil {
			errs = append(errs, err)
		} else if len(errs) == 0 {
			m.PHP = set
		}
	}
	if err := errors.Join(errs...); err != nil {
		return m, fmt.Errorf("invalid %s:\n%w", FileName, err)
	}
	return m, nil
}

func cleanDocroot(d string) (string, error) {
	c := filepath.Clean(strings.TrimSpace(d))
	if d == "" || filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("docroot %q must be a folder inside the repository (e.g. \"public\" or \".\")", d)
	}
	for _, seg := range strings.Split(c, "/") {
		if seg != "." && strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("docroot %q must not be a hidden folder", d)
		}
	}
	return c, nil
}

// cleanRel turns a repository-relative path into its clean form. It must
// name something inside the repository (not "." and not "..").
func cleanRel(p string) (string, bool) {
	s := strings.TrimSpace(p)
	c := filepath.ToSlash(filepath.Clean(s))
	if s == "" || filepath.IsAbs(s) || c == "." || c == ".." || strings.HasPrefix(c, "../") || strings.ContainsRune(s, 0) {
		return "", false
	}
	return c, true
}

// within reports whether p is dir or lies below it.
func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

func cleanPersist(list []string, docroot string) ([]string, []error) {
	var errs []error
	if len(list) > maxPersist {
		return nil, []error{fmt.Errorf("persist lists more than %d folders", maxPersist)}
	}
	var out []string
	for _, p := range list {
		c, ok := cleanRel(p)
		if !ok {
			errs = append(errs, fmt.Errorf("persist folder %q must be a folder inside the repository, e.g. \"public/uploads\"", p))
			continue
		}
		if within(docroot, c) {
			errs = append(errs, fmt.Errorf("persist folder %q contains the docroot %q: persist folders are linked to DATA_DIR, so the docroot can't be one", c, docroot))
			continue
		}
		for _, o := range out {
			if within(c, o) || within(o, c) {
				errs = append(errs, fmt.Errorf("persist folders %q and %q overlap", o, c))
			}
		}
		out = append(out, c)
	}
	return out, errs
}

func cleanBeforeStart(list []string, persist []string) ([]string, []error) {
	var errs []error
	if len(list) > maxBeforeStart {
		return nil, []error{fmt.Errorf("before_start lists more than %d scripts", maxBeforeStart)}
	}
	var out []string
	for _, s := range list {
		c, ok := cleanRel(s)
		if !ok || !strings.HasSuffix(c, ".php") {
			errs = append(errs, fmt.Errorf("before_start script %q must be a .php file inside the repository, e.g. \"bin/migrate.php\"", s))
			continue
		}
		inPersist := false
		for _, p := range persist {
			inPersist = inPersist || within(c, p)
		}
		if inPersist {
			errs = append(errs, fmt.Errorf("before_start script %q is inside a persist folder, which is linked to DATA_DIR and never runs", c))
			continue
		}
		out = append(out, c)
	}
	return out, errs
}
