// Package app defines the runtime description of a hosted app and the
// validation rules shared by the dev config, the dashboard and the deployer.
package app

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ContractVersion is the app contract version phpboat implements
// (docs/app-contract.md).
const ContractVersion = 1

// Default resource limits (docs/app-contract.md §9).
const (
	DefaultMemoryMax   = 256 << 20
	DefaultCPUMax      = 1.0
	DefaultPidsMax     = 256
	DefaultNoFile      = 4096
	DefaultHealthPath  = "/"
	DefaultConcurrency = 4  // PHP workers (pm.max_children)
	MaxConcurrency     = 16 // keep in sync with php.MaxConcurrency
)

// Limits are the per-app runtime resource limits.
type Limits struct {
	MemoryMax int64   // bytes (memory.max)
	CPUMax    float64 // cores, e.g. 1.0 or 0.5
	PidsMax   int
}

// Spec is everything the supervisor needs to run one app: an FPM pool
// for the deployed build, behind phpserve.
type Spec struct {
	Name       string
	Domain     string
	BuildID    string // the deployed build ("" = never deployed)
	CommitSHA  string // full SHA of the commit the build was made from
	BuildDir   string // the deployed build (absolute, read-only)
	Docroot    string // public folder relative to BuildDir ("public", ".")
	HealthPath string // GET through FastCGI must answer 2xx/3xx
	// BeforeStart are PHP scripts (relative to BuildDir) run in order before
	// every start of the app (docs/app-contract.md §8).
	BeforeStart []string
	PHP         map[string]string // the [php] settings of phpboat.toml (validated by the manifest)
	Concurrency int               // PHP workers
	Env         map[string]string // user env vars (reserved names rejected)
	Limits      Limits
}

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,22}[a-z0-9]$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ReservedEnv are env names set by phpboat that users may not override.
var ReservedEnv = []string{"DATA_DIR", "TMPDIR"}

// ValidateName checks an app name: 2-24 chars, lowercase letters, digits and
// hyphens, starting with a letter and not ending with a hyphen. The limit
// keeps the system user name "phpboat-<name>" within 32 characters, the
// longest name useradd accepts.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid app name %q: use 2-24 characters of a-z, 0-9 and '-', starting with a letter and not ending with '-'", name)
	}
	return nil
}

// NameFromRepo derives the app name from a repository name:
// lowercased, with '_' and '.' turned into '-'. The result must still pass
// ValidateName.
func NameFromRepo(repo string) (string, error) {
	n := strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(repo))
	if err := ValidateName(n); err != nil {
		return "", fmt.Errorf("the repository name %q cannot be used as an app name (%q): app names are 2-24 characters of a-z, 0-9 and '-', starting with a letter and not ending with '-'", repo, n)
	}
	return n, nil
}

// ValidateEnvName checks a user-supplied env var name.
func ValidateEnvName(name string) error {
	if !envNameRe.MatchString(name) {
		return fmt.Errorf("invalid env var name %q: use letters, digits and '_', not starting with a digit", name)
	}
	if strings.HasPrefix(name, "PHPBOAT_") {
		return fmt.Errorf("env var %q is reserved (names starting with PHPBOAT_ are set by phpboat)", name)
	}
	for _, r := range ReservedEnv {
		if name == r {
			return fmt.Errorf("env var %q is reserved (set by phpboat)", name)
		}
	}
	return nil
}

// Validate checks a Spec for internal consistency.
func (s *Spec) Validate() error {
	var errs []error
	if err := ValidateName(s.Name); err != nil {
		errs = append(errs, err)
	}
	// An app without a build (never deployed) has no build dir yet.
	if s.BuildID != "" {
		if !strings.HasPrefix(s.BuildDir, "/") {
			errs = append(errs, fmt.Errorf("app %q: build dir must be an absolute path", s.Name))
		}
		if s.Docroot == "" || strings.HasPrefix(s.Docroot, "/") || s.Docroot == ".." || strings.HasPrefix(s.Docroot, "../") {
			errs = append(errs, fmt.Errorf("app %q: docroot %q must be a folder inside the build", s.Name, s.Docroot))
		}
		for _, b := range s.BeforeStart {
			if !strings.HasSuffix(b, ".php") || strings.HasPrefix(b, "/") || strings.Contains(b, "..") {
				errs = append(errs, fmt.Errorf("app %q: before_start %q must be a .php file inside the build", s.Name, b))
			}
		}
	}
	if s.Concurrency == 0 {
		s.Concurrency = DefaultConcurrency
	}
	if s.Concurrency < 1 || s.Concurrency > MaxConcurrency {
		errs = append(errs, fmt.Errorf("app %q: PHP workers must be between 1 and %d", s.Name, MaxConcurrency))
	}
	if s.HealthPath == "" {
		s.HealthPath = DefaultHealthPath
	}
	if !strings.HasPrefix(s.HealthPath, "/") {
		errs = append(errs, fmt.Errorf("app %q: health path must start with '/'", s.Name))
	}
	for k := range s.Env {
		if err := ValidateEnvName(k); err != nil {
			errs = append(errs, fmt.Errorf("app %q: %w", s.Name, err))
		}
	}
	if s.Limits.MemoryMax < 16<<20 {
		errs = append(errs, fmt.Errorf("app %q: memory limit must be at least 16M", s.Name))
	}
	if s.Limits.CPUMax <= 0 || s.Limits.CPUMax > 64 {
		errs = append(errs, fmt.Errorf("app %q: cpu limit must be between 0 and 64 cores", s.Name))
	}
	if s.Limits.PidsMax < 16 {
		errs = append(errs, fmt.Errorf("app %q: pids limit must be at least 16", s.Name))
	}
	return errors.Join(errs...)
}

// ParseBytes parses sizes like "256M", "1G", "512K" or a plain byte count.
// Units are binary (1K = 1024).
func ParseBytes(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "B")
	t = strings.TrimSuffix(t, "I") // accept "Mi", "Gi"
	mult := int64(1)
	if n := len(t); n > 0 {
		switch t[n-1] {
		case 'K':
			mult, t = 1<<10, t[:n-1]
		case 'M':
			mult, t = 1<<20, t[:n-1]
		case 'G':
			mult, t = 1<<30, t[:n-1]
		}
	}
	v, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q (examples: 256M, 1G)", s)
	}
	return v * mult, nil
}
