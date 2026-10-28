package php

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Defaults (docs/plan.md §8).
const (
	DefaultConcurrency = 4
	MaxConcurrency     = 16
	opcacheMB          = 32
)

// Settings is the [php] table of phpboat.toml: a short allowlist.
// Keys are the phpboat.toml names; values are validated by ValidateSettings.
type Settings map[string]string

// settingRules: phpboat.toml key → php.ini name and a value check.
var settingRules = map[string]struct {
	ini   string
	check func(string) error
}{
	"memory_limit":        {"memory_limit", size},
	"max_execution_time":  {"max_execution_time", intRange(0, 3600)},
	"upload_max_filesize": {"upload_max_filesize", size},
	"max_input_vars":      {"max_input_vars", intRange(1, 100000)},
	"timezone":            {"date.timezone", timezone},
}

// SettingNames lists the allowed [php] keys.
func SettingNames() []string {
	var out []string
	for k := range settingRules {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var sizeRe = regexp.MustCompile(`^[1-9][0-9]{0,6}[KMG]?$`)
var tzRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9_+\-]+){0,2}$`)

func size(v string) error {
	if !sizeRe.MatchString(v) {
		return fmt.Errorf("%q is not a size like 64M or 2G", v)
	}
	if b := sizeBytes(v); b > 4<<30 {
		return fmt.Errorf("%q is more than 4G", v)
	}
	return nil
}

func sizeBytes(v string) int64 {
	mult := int64(1)
	switch v[len(v)-1] {
	case 'K':
		mult, v = 1<<10, v[:len(v)-1]
	case 'M':
		mult, v = 1<<20, v[:len(v)-1]
	case 'G':
		mult, v = 1<<30, v[:len(v)-1]
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n * mult
}

func intRange(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi || strconv.Itoa(n) != v {
			return fmt.Errorf("%q must be a whole number from %d to %d", v, lo, hi)
		}
		return nil
	}
}

func timezone(v string) error {
	if !tzRe.MatchString(v) {
		return fmt.Errorf("%q is not a time zone like UTC or Europe/Berlin", v)
	}
	return nil
}

// ValidateSettings checks every key and value, reporting all problems.
func ValidateSettings(s Settings) error {
	var errs []error
	for _, k := range sortedKeys(s) {
		r, ok := settingRules[k]
		if !ok {
			errs = append(errs, fmt.Errorf("[php] %s is not supported (allowed: %s)", k, strings.Join(SettingNames(), ", ")))
			continue
		}
		if err := r.check(s[k]); err != nil {
			errs = append(errs, fmt.Errorf("[php] %s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DefaultMemoryLimit is the per-request memory_limit: the app's memory
// divided by its concurrency, so concurrent requests fail with PHP's own
// error before the kernel OOM-kills workers (phase 1, F4). At least 16M.
func DefaultMemoryLimit(memoryMax int64, concurrency int) string {
	if concurrency < 1 {
		concurrency = 1
	}
	mb := memoryMax / int64(concurrency) >> 20
	if mb < 16 {
		mb = 16
	}
	return strconv.FormatInt(mb, 10) + "M"
}

// MemoryLimitBytes is memory_limit in bytes for a valid value.
func MemoryLimitBytes(v string) int64 { return sizeBytes(v) }

// Pool is one app's FPM configuration.
type Pool struct {
	App         string
	Build       string // the deployed build directory (open_basedir)
	DataDir     string
	TmpDir      string
	SessionsDir string
	Socket      string
	Concurrency int
	MemoryMax   int64 // the app's cgroup memory limit
	Settings    Settings
}

// INI returns the php_admin_value settings of the pool, sorted.
func (p Pool) INI() map[string]string {
	ini := map[string]string{
		"display_errors":                  "0",
		"display_startup_errors":          "0",
		"log_errors":                      "1",
		"expose_php":                      "0",
		"open_basedir":                    strings.Join([]string{p.Build, p.DataDir, p.TmpDir, p.SessionsDir}, ":"),
		"sys_temp_dir":                    p.TmpDir,
		"upload_tmp_dir":                  p.TmpDir,
		"session.save_path":               p.SessionsDir,
		"session.gc_probability":          "0", // phpboat cleans sessions/ itself
		"opcache.enable":                  "1",
		"opcache.validate_timestamps":     "0", // builds are immutable; a deploy restarts the pool
		"opcache.memory_consumption":      strconv.Itoa(opcacheMB),
		"opcache.interned_strings_buffer": "4",
		"opcache.jit":                     "disable",
		"opcache.jit_buffer_size":         "0",
		"realpath_cache_size":             "1M",
		"allow_url_include":               "0",
		"memory_limit":                    DefaultMemoryLimit(p.MemoryMax, p.Concurrency),
		"max_execution_time":              "30",
		"upload_max_filesize":             "20M",
		"post_max_size":                   "20M",
		"max_input_vars":                  "1000",
		"date.timezone":                   "UTC",
	}
	for k, v := range p.Settings {
		r, ok := settingRules[k]
		if !ok {
			continue
		}
		ini[r.ini] = v
		if k == "upload_max_filesize" {
			ini["post_max_size"] = v
		}
	}
	return ini
}

// Config renders the php-fpm configuration file (used with -F -O -y).
// Secrets never go in it: env vars reach PHP through clear_env = no.
func (p Pool) Config() (string, error) {
	if p.Concurrency < 1 || p.Concurrency > MaxConcurrency {
		return "", fmt.Errorf("concurrency %d out of range 1-%d", p.Concurrency, MaxConcurrency)
	}
	if err := ValidateSettings(p.Settings); err != nil {
		return "", err
	}
	// Paths go into the ini file and open_basedir (":"-separated).
	for _, v := range []string{p.Build, p.DataDir, p.TmpDir, p.SessionsDir, p.Socket} {
		if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, "\n\r\"';:=[] ") {
			return "", fmt.Errorf("php: unusable path %q in the pool config", v)
		}
	}
	var b strings.Builder
	b.WriteString("; Written by phpboat for app " + p.App + ". Changes are overwritten on every start.\n")
	b.WriteString("[global]\n")
	b.WriteString("error_log = /proc/self/fd/2\n")
	b.WriteString("log_limit = 16384\n")
	b.WriteString("systemd_interval = 0\n")
	b.WriteString("process_control_timeout = 10s\n")
	b.WriteString("\n[app]\n")
	fmt.Fprintf(&b, "listen = %s\n", p.Socket)
	b.WriteString("listen.backlog = 256\n")
	b.WriteString("pm = ondemand\n")
	fmt.Fprintf(&b, "pm.max_children = %d\n", p.Concurrency)
	b.WriteString("pm.process_idle_timeout = 30s\n")
	b.WriteString("pm.max_requests = 500\n")
	b.WriteString("request_terminate_timeout = 95s\n")
	b.WriteString("clear_env = no\n")
	b.WriteString("catch_workers_output = yes\n")
	b.WriteString("decorate_workers_output = no\n")
	b.WriteString("security.limit_extensions = .php\n")
	ini := p.INI()
	for _, k := range sortedKeys(ini) {
		fmt.Fprintf(&b, "php_admin_value[%s] = %s\n", k, ini[k])
	}
	return b.String(), nil
}
