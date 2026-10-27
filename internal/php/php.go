// Package php knows about the host's PHP (docs/plan.md §3, §8): which
// php-fpm is installed, the pool configuration phpboat writes for each
// app, and when apt updated PHP underneath the running pools.
package php

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runtime is one installed PHP version.
type Runtime struct {
	Version string // "8.5"
	FPM     string // /usr/sbin/php-fpm8.5
	CLI     string // /usr/bin/php8.5
}

// Package is the Debian package of the FPM binary.
func (r Runtime) Package() string { return "php" + r.Version + "-fpm" }

// ErrNotInstalled means no php-fpm was found.
var ErrNotInstalled = errors.New("php-fpm is not installed: run the phpboat installer again (it installs PHP from Ubuntu)")

var fpmRe = regexp.MustCompile(`^php-fpm(\d+)\.(\d+)$`)

// Detect finds the newest php-fpm under root ("" = /): /usr/sbin/php-fpmX.Y.
// Ubuntu ships exactly one PHP per release; if several are installed
// (a PPA), the newest wins.
func Detect(root string) (Runtime, error) {
	dir := filepath.Join(root, "/usr/sbin")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Runtime{}, ErrNotInstalled
	}
	type cand struct {
		major, minor int
		name         string
	}
	var cs []cand
	for _, e := range entries {
		m := fpmRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		maj, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		cs = append(cs, cand{maj, min, e.Name()})
	}
	if len(cs) == 0 {
		return Runtime{}, ErrNotInstalled
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].major != cs[j].major {
			return cs[i].major > cs[j].major
		}
		return cs[i].minor > cs[j].minor
	})
	c := cs[0]
	v := fmt.Sprintf("%d.%d", c.major, c.minor)
	return Runtime{Version: v, FPM: filepath.Join("/usr/sbin", c.name), CLI: "/usr/bin/php" + v}, nil
}

// Build identifies the installed FPM build: the package version from dpkg
// ("8.5.4-0ubuntu1.5"), else the binary's size and modification time.
// A change means apt replaced PHP and running pools still use the old one.
func (r Runtime) Build(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "dpkg-query", "-W", "-f=${Version}", r.Package()).Output()
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return v
	}
	if st, err := os.Stat(r.FPM); err == nil {
		return fmt.Sprintf("binary %d bytes, %s", st.Size(), st.ModTime().UTC().Format(time.RFC3339))
	}
	return ""
}

// Watch calls changed(old, new) when Build changes, checking every
// interval until ctx ends.
func (r Runtime) Watch(ctx context.Context, interval time.Duration, changed func(old, new string)) {
	last := r.Build(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if cur := r.Build(ctx); cur != "" && last != "" && cur != last {
			changed(last, cur)
			last = cur
		} else if cur != "" {
			last = cur
		}
	}
}
