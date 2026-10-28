// Package phpcli runs PHP CLI commands for an app: Composer while a build is
// prepared, and before_start scripts before a start. The command runs as the
// app's user, inside a cgroup, with exactly the environment it is given, and
// is killed when it outlives its timeout (docs/plan.md §5, §8).
package phpcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/users"
)

// Command is one php invocation.
type Command struct {
	Binary string            // the php CLI, e.g. /usr/bin/php8.5
	Ini    map[string]string // php -d key=value settings
	Args   []string          // the script and its arguments
	Dir    string            // working directory
	Env    []string          // the complete environment ("KEY=value")
	User   users.User        // the app user the process runs as
	Group  *cgroup.Group     // the cgroup the process starts in
	Log    *logs.Log         // receives every output line (may be nil)
}

// Result is how a command ended.
type Result struct {
	ExitCode int    // -1 if it was killed by a signal or by the timeout
	TimedOut bool   // ctx ended before the command did, and it was killed
	Tail     string // the last lines of output (for error messages)
}

// tailSize bounds the output kept for error messages.
const tailSize = 4096

// Run executes c and waits for it. Its output goes to c.Log line by line.
// An error means the command could not be started at all; a non-zero exit
// is reported in the Result.
func Run(ctx context.Context, c Command) (Result, error) {
	fd, err := c.Group.OpenFD()
	if err != nil {
		return Result{}, err
	}
	defer unix.Close(fd)

	r, w, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	defer r.Close()
	// Like php-fpm's pipes (phase 1, F2), the write end belongs to the app user.
	if err := w.Chown(int(c.User.UID), int(c.User.GID)); err != nil {
		w.Close()
		return Result{}, err
	}

	args := make([]string, 0, 2*len(c.Ini)+len(c.Args))
	keys := make([]string, 0, len(c.Ini))
	for k := range c.Ini {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-d", k+"="+c.Ini[k])
	}
	args = append(args, c.Args...)

	cmd := exec.Command(c.Binary, args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: c.User.UID, Gid: c.User.GID, Groups: []uint32{}},
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true, // clone3(CLONE_INTO_CGROUP): the process starts inside its cgroup
		CgroupFD:    fd,
	}
	if err := cmd.Start(); err != nil {
		w.Close()
		return Result{}, fmt.Errorf("start %s: %w", c.Binary, err)
	}
	w.Close()

	tail := &ring{max: tailSize}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		src := io.TeeReader(r, tail)
		if c.Log != nil {
			c.Log.Capture(logs.Stdout, src)
			return
		}
		io.Copy(io.Discard, src)
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	res := Result{}
	select {
	case <-waited:
	case <-ctx.Done():
		res.TimedOut = true
		c.Group.Kill()
		<-waited
	}
	// Processes the command started (git, for one) can keep the output pipe
	// open after it exited. Kill them so the copy ends.
	c.Group.Kill()
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c.Group.WaitEmpty(wctx)
	cancel()
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
	}

	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode = ps.ExitCode()
	}
	res.Tail = tail.String()
	return res, nil
}

// LastLine returns the last non-empty line of s (for one-line error messages).
func LastLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	last := ""
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			last = t
		}
	}
	return last
}

// ring keeps the last max bytes written to it.
type ring struct {
	max int
	buf []byte
}

func (r *ring) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = r.buf[over:]
	}
	return len(p), nil
}

func (r *ring) String() string { return string(r.buf) }
