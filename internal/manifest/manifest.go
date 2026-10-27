// Package manifest reads an app's optional phpboat.toml
// (docs/app-contract.md §3) and validates it against the release.
package manifest

import (
	"errors"
	"fmt"
	"io"
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

const maxSize = 64 << 10

// Manifest is a validated phpboat.toml (or the defaults without one).
type Manifest struct {
	Contract   int          `json:"contract"`
	Docroot    string       `json:"docroot"` // "public", "." or another folder, relative to the root
	HealthPath string       `json:"health_path"`
	PHP        php.Settings `json:"php,omitempty"`
}

// Default is the manifest of a repo without phpboat.toml.
func Default() Manifest {
	return Manifest{Contract: app.ContractVersion, Docroot: DefaultDocroot, HealthPath: app.DefaultHealthPath}
}

type raw struct {
	Contract   *int           `toml:"contract"`
	Docroot    *string        `toml:"docroot"`
	HealthPath *string        `toml:"health_path"`
	PHP        map[string]any `toml:"php"`
}

// later lists contract keys this phpboat does not implement yet, so they
// fail with a clear message instead of "unknown key".
var later = map[string]bool{"persist": true, "before_start": true, "cron": true}

// Load reads dir/phpboat.toml (defaults if there is none) and checks that
// the docroot exists in dir.
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
	return m, checkDocroot(dir, m.Docroot)
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
