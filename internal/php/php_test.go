package php

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetect(t *testing.T) {
	root := t.TempDir()
	if _, err := Detect(root); err != ErrNotInstalled {
		t.Errorf("empty root: %v", err)
	}
	sbin := filepath.Join(root, "usr/sbin")
	os.MkdirAll(sbin, 0o755)
	for _, n := range []string{"php-fpm8.3", "php-fpm8.10", "php-fpm7.4", "php-fpm", "php-fpm8.5.bak"} {
		os.WriteFile(filepath.Join(sbin, n), nil, 0o755)
	}
	r, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "8.10" || r.FPM != "/usr/sbin/php-fpm8.10" || r.CLI != "/usr/bin/php8.10" || r.Package() != "php8.10-fpm" {
		t.Errorf("%+v", r)
	}
}

func TestDefaultMemoryLimit(t *testing.T) {
	for _, c := range []struct {
		mem  int64
		conc int
		want string
	}{{256 << 20, 4, "64M"}, {512 << 20, 4, "128M"}, {256 << 20, 1, "256M"}, {32 << 20, 16, "16M"}, {256 << 20, 0, "256M"}} {
		if got := DefaultMemoryLimit(c.mem, c.conc); got != c.want {
			t.Errorf("%d/%d: %s, want %s", c.mem>>20, c.conc, got, c.want)
		}
	}
}

func TestValidateSettings(t *testing.T) {
	ok := Settings{"memory_limit": "128M", "max_execution_time": "60", "upload_max_filesize": "50M", "max_input_vars": "3000", "timezone": "America/Argentina/Buenos_Aires"}
	if err := ValidateSettings(ok); err != nil {
		t.Error(err)
	}
	for k, v := range map[string]string{
		"memory_limit": "-1", "max_execution_time": "1e3", "upload_max_filesize": "8G",
		"max_input_vars": "0", "timezone": "UTC\nopen_basedir = /", "display_errors": "1",
	} {
		if err := ValidateSettings(Settings{k: v}); err == nil {
			t.Errorf("%s = %q accepted", k, v)
		}
	}
}

func pool() Pool {
	return Pool{App: "blog", Build: "/var/lib/phpboat/apps/blog/releases/v1", DataDir: "/var/lib/phpboat/apps/blog/data",
		TmpDir: "/var/lib/phpboat/apps/blog/tmp", SessionsDir: "/var/lib/phpboat/apps/blog/sessions",
		Socket: "/var/lib/phpboat/apps/blog/run/php.sock", Concurrency: 4, MemoryMax: 256 << 20}
}

func TestConfig(t *testing.T) {
	p := pool()
	p.Settings = Settings{"upload_max_filesize": "50M", "timezone": "Asia/Kolkata"}
	c, err := p.Config()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"listen = /var/lib/phpboat/apps/blog/run/php.sock\n", "pm = ondemand\n", "pm.max_children = 4\n",
		"clear_env = no\n", "systemd_interval = 0\n", "error_log = /proc/self/fd/2\n", "security.limit_extensions = .php\n",
		"php_admin_value[memory_limit] = 64M\n", "php_admin_value[upload_max_filesize] = 50M\n", "php_admin_value[post_max_size] = 50M\n",
		"php_admin_value[date.timezone] = Asia/Kolkata\n", "php_admin_value[opcache.validate_timestamps] = 0\n",
		"php_admin_value[open_basedir] = /var/lib/phpboat/apps/blog/releases/v1:/var/lib/phpboat/apps/blog/data:/var/lib/phpboat/apps/blog/tmp:/var/lib/phpboat/apps/blog/sessions\n",
		"php_admin_value[display_errors] = 0\n",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	for _, bad := range []string{"\nuser =", "\nerror_log = /dev", "php_admin_value[error_log]"} {
		if strings.Contains(c, bad) {
			t.Errorf("config contains %q", bad)
		}
	}
	p.Settings = Settings{"memory_limit": "200M"}
	if c, _ := p.Config(); !strings.Contains(c, "php_admin_value[memory_limit] = 200M\n") {
		t.Error("memory_limit override ignored")
	}
}

func TestConfigRejects(t *testing.T) {
	for name, f := range map[string]func(*Pool){
		"concurrency 0":  func(p *Pool) { p.Concurrency = 0 },
		"concurrency 17": func(p *Pool) { p.Concurrency = 17 },
		"bad setting":    func(p *Pool) { p.Settings = Settings{"short_open_tag": "1"} },
		"newline value":  func(p *Pool) { p.Settings = Settings{"timezone": "UTC\n[evil]"} },
		"relative path":  func(p *Pool) { p.Build = "releases/v1" },
		"colon in path":  func(p *Pool) { p.DataDir = "/a:/b" },
		"newline path":   func(p *Pool) { p.Socket = "/x\nlisten = 0.0.0.0:9000" },
	} {
		p := pool()
		f(&p)
		if _, err := p.Config(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWatch(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "php-fpm9.9")
	os.WriteFile(bin, []byte("one"), 0o755)
	r := Runtime{Version: "9.9", FPM: bin} // no dpkg package: falls back to the binary's size and time
	changed := make(chan [2]string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Watch(ctx, 20*time.Millisecond, func(o, n string) { changed <- [2]string{o, n} })
	time.Sleep(60 * time.Millisecond)
	os.WriteFile(bin, []byte("two, longer"), 0o755)
	select {
	case c := <-changed:
		if c[0] == c[1] || !strings.Contains(c[1], "11 bytes") {
			t.Errorf("%q", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no change reported")
	}
}
