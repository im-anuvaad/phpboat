package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is spike.json.
type Config struct {
	Root string      `json:"root"` // e.g. /var/lib/phpboat-spike
	Apps []AppConfig `json:"apps"`
}

type AppConfig struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	PHP      string `json:"php"` // "8.5" (host) or "8.3" (the noble rootfs)
	Src      string `json:"src"` // the sample's directory (stands in for the GitHub tarball)
	MemoryMB int    `json:"memory_mb"`
	Workers  int    `json:"workers"`
}

// Manifest is the phpboat.toml subset the spike understands.
type Manifest struct {
	Contract    int               `toml:"contract"`
	Docroot     string            `toml:"docroot"`
	HealthPath  string            `toml:"health_path"`
	Persist     []string          `toml:"persist"`
	BeforeStart []string          `toml:"before_start"`
	PHP         map[string]string `toml:"php"`
	Cron        []struct {
		Schedule string `toml:"schedule"`
		Run      string `toml:"run"`
	} `toml:"cron"`
}

// Runtime is one PHP installation.
type Runtime struct {
	Chroot string // "" = host
	FPM    string
	CLI    string
}

var runtimes = map[string]Runtime{
	"8.5": {FPM: "/usr/sbin/php-fpm8.5", CLI: "/usr/bin/php8.5"},
	"8.3": {Chroot: "/srv/noble", FPM: "/usr/sbin/php-fpm8.3", CLI: "/usr/bin/php8.3"},
}

const composerPhar = "/usr/local/lib/phpboat/composer.phar"
const cgRoot = "/sys/fs/cgroup/phpboat-spike"

type App struct {
	AppConfig
	Root     string
	M        Manifest
	RT       Runtime
	UID, GID uint32

	envOverride map[string]string // extra env (the build gets HOME/COMPOSER_HOME)
}

func loadConfig(p string) (*Config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) app(ac AppConfig) (*App, error) {
	a := &App{AppConfig: ac, Root: c.Root}
	if a.MemoryMB == 0 {
		a.MemoryMB = 256
	}
	if a.Workers == 0 {
		a.Workers = 4
	}
	rt, ok := runtimes[ac.PHP]
	if !ok {
		return nil, fmt.Errorf("%s: unknown php %q", ac.Name, ac.PHP)
	}
	a.RT = rt
	a.M = Manifest{Docroot: "public", HealthPath: "/"}
	if b, err := os.ReadFile(filepath.Join(ac.Src, "phpboat.toml")); err == nil {
		md, err := toml.Decode(string(b), &a.M)
		if err != nil {
			return nil, fmt.Errorf("%s: phpboat.toml: %w", ac.Name, err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return nil, fmt.Errorf("%s: phpboat.toml: unknown keys %v", ac.Name, u)
		}
	}
	if u, err := user.Lookup(a.User()); err == nil {
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		a.UID, a.GID = uint32(uid), uint32(gid)
	}
	return a, nil
}

func (a *App) User() string { return "phpboat-" + a.Name }
func (a *App) Dir(parts ...string) string {
	return filepath.Join(append([]string{a.Root, "apps", a.Name}, parts...)...)
}
func (a *App) Build() string   { return a.Dir("build") }
func (a *App) Docroot() string { return filepath.Clean(filepath.Join(a.Build(), a.M.Docroot)) }
func (a *App) Sock() string    { return a.hostPath(a.Dir("run", "php.sock")) }
func (a *App) Cgroup() string  { return filepath.Join(cgRoot, a.Name) }

// hostPath maps a path as PHP sees it to the host (the chroot bind-mounts
// the spike root at the same path, so it is the same file either way).
func (a *App) hostPath(p string) string { return p }

func (a *App) env() []string {
	env := map[string]string{
		"PATH":             "/usr/local/bin:/usr/bin:/bin",
		"LANG":             "C.UTF-8",
		"DATA_DIR":         a.Dir("data"),
		"TMPDIR":           a.Dir("tmp"),
		"PHPBOAT_APP":      a.Name,
		"PHPBOAT_DOMAIN":   a.Host,
		"PHPBOAT_RELEASE":  "spike",
		"PHPBOAT_CONTRACT": "1",
		"SECRET_FROM_DASH": "visible-to-getenv", // stands in for a dashboard env var
	}
	for k, v := range a.envOverride {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// poolConf renders the FPM config (docs/plan.md §8).
func (a *App) poolConf() string {
	ini := map[string]string{
		"display_errors":                  "0",
		"log_errors":                      "1",
		"expose_php":                      "0",
		"open_basedir":                    strings.Join([]string{a.Build(), a.Dir("data"), a.Dir("tmp"), a.Dir("sessions")}, ":"),
		"sys_temp_dir":                    a.Dir("tmp"),
		"upload_tmp_dir":                  a.Dir("tmp"),
		"session.save_path":               a.Dir("sessions"),
		"opcache.enable":                  "1",
		"opcache.validate_timestamps":     "0",
		"opcache.memory_consumption":      "32",
		"opcache.interned_strings_buffer": "4",
		"opcache.jit":                     "disable",
		"opcache.jit_buffer_size":         "0",
		"realpath_cache_size":             "1M",
		"allow_url_include":               "0",
		"memory_limit":                    "128M",
		"max_execution_time":              "30",
		"upload_max_filesize":             "20M",
		"post_max_size":                   "20M",
		"date.timezone":                   "UTC",
	}
	for k, v := range a.M.PHP {
		switch k {
		case "upload_max_filesize":
			ini[k], ini["post_max_size"] = v, v
		case "timezone":
			ini["date.timezone"] = v
		default:
			ini[k] = v
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[global]\nerror_log = /proc/self/fd/2\nlog_limit = 16384\nprocess_control_timeout = 10s\n\n[app]\n")
	fmt.Fprintf(&b, "listen = %s\nlisten.backlog = 256\n", a.Dir("run", "php.sock"))
	fmt.Fprintf(&b, "pm = ondemand\npm.max_children = %d\npm.process_idle_timeout = 30s\npm.max_requests = 500\n", a.Workers)
	fmt.Fprintf(&b, "pm.status_path = /__phpboat_fpm_status\nrequest_terminate_timeout = 95s\n")
	fmt.Fprintf(&b, "clear_env = no\ncatch_workers_output = yes\ndecorate_workers_output = no\n")
	keys := make([]string, 0, len(ini))
	for k := range ini {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "php_admin_value[%s] = %s\n", k, ini[k])
	}
	return b.String()
}

// spawn starts argv in the app's cgroup, as its user, optionally chrooted.
func (a *App) spawn(cg string, dir string, out io.Writer, argv ...string) (*exec.Cmd, error) {
	fd, err := syscall.Open(cg, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	// Our own pipe, owned by the app user: FPM reopens /proc/self/fd/2 for
	// its error_log, which fails on a root-owned pipe (EACCES).
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if err := pw.Chown(int(a.UID), int(a.GID)); err != nil {
		pr.Close()
		pw.Close()
		return nil, err
	}
	go func() { io.Copy(out, pr); pr.Close() }()
	defer pw.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = a.env()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:      a.RT.Chroot,
		Credential:  &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: []uint32{}},
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true,
		CgroupFD:    fd,
	}
	return cmd, cmd.Start()
}

// runPHP runs a PHP script with the CLI in cg and returns its peak RSS.
func (a *App) runPHP(cg string, out io.Writer, timeout time.Duration, args ...string) (maxRSSKB int64, dur time.Duration, err error) {
	start := time.Now()
	cmd, err := a.spawn(cg, a.Build(), out, append([]string{a.RT.CLI}, args...)...)
	if err != nil {
		return 0, 0, err
	}
	t := time.AfterFunc(timeout, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	err = cmd.Wait()
	t.Stop()
	if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		maxRSSKB = ru.Maxrss
	}
	return maxRSSKB, time.Since(start), err
}

func writeCg(dir, file, val string) error {
	return os.WriteFile(filepath.Join(dir, file), []byte(val), 0o644)
}

func ensureCgroup(dir string, memMB int) error {
	if err := os.MkdirAll(cgRoot, 0o755); err != nil {
		return err
	}
	if err := writeCg(cgRoot, "cgroup.subtree_control", "+memory +cpu +pids"); err != nil {
		return fmt.Errorf("enable controllers: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for k, v := range map[string]string{
		"memory.max":      strconv.Itoa(memMB << 20),
		"memory.swap.max": "0",
		"cpu.max":         "100000 100000",
		"pids.max":        "256",
	} {
		if err := writeCg(dir, k, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

func readCgInt(dir, file string) int64 {
	b, _ := os.ReadFile(filepath.Join(dir, file))
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// runIn runs argv in cg with cwd dir and waits (the build stage).
func (a *App) runIn(cg, dir string, out io.Writer, timeout time.Duration, argv ...string) error {
	cmd, err := a.spawn(cg, dir, out, argv...)
	if err != nil {
		return err
	}
	t := time.AfterFunc(timeout, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	defer t.Stop()
	return cmd.Wait()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
