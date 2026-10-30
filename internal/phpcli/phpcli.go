// Package phpcli runs PHP CLI commands for an app: Composer while a build is
// prepared, before_start scripts before a start, and cron jobs. The command
// runs as the app's user, inside a cgroup, with exactly the environment it is
// given, and is killed when it outlives its timeout (docs/architecture.md §5, §8).
package phpcli

import (
	"bufio"
	"context"
	"errors"
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
	// Prefix is put in front of every output line in Log (cron jobs use "cron:<n> ").
	Prefix string
	// SharedGroup is set when other processes live in Group as well (a cron
	// job runs next to its app's PHP-FPM workers). Then only the command's own
	// process group is killed, when it times out and when it has exited; the
	// cgroup is left alone.
	SharedGroup bool
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
	if ctx.Err() != nil {
		return Result{ExitCode: -1, TimedOut: true}, nil
	}
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
	// Like php-fpm's pipes (docs/architecture.md §8), the write end belongs to the app user.
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
	pid := cmd.Process.Pid

	tail := &ring{max: tailSize}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		src := io.TeeReader(r, tail)
		if c.Log != nil {
			capture(c.Log, c.Prefix, src)
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
		c.kill(pid)
		<-waited
	}
	// Processes the command started (git, for one) can keep the output pipe
	// open after it exited. Kill them so the copy ends.
	c.kill(pid)
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c.waitGone(wctx, pid)
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

// kill stops the command and whatever it still runs: its whole cgroup, or
// only its process group in a shared cgroup.
func (c Command) kill(pid int) {
	if c.SharedGroup {
		// Setpgid made the command the leader of its own process group.
		_ = syscall.Kill(-pid, syscall.SIGKILL) // ESRCH: nothing left
		return
	}
	c.Group.Kill()
}

// waitGone waits until nothing of the command is left (or ctx ends).
func (c Command) waitGone(ctx context.Context, pid int) {
	if !c.SharedGroup {
		c.Group.WaitEmpty(ctx)
		return
	}
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// capture reads lines from r until EOF and writes each to l, prefixed. Lines
// longer than logs.MaxLineBytes are split, as logs.Log.Capture does.
func capture(l *logs.Log, prefix string, r io.Reader) {
	br := bufio.NewReaderSize(r, logs.MaxLineBytes)
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(chunk) > 0 || (err == nil && !isPrefix) {
			l.Write(logs.Stdout, prefix+strings.TrimSuffix(string(chunk), "\r"))
		}
		if err != nil {
			return
		}
	}
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
