package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/im-anuvaad/phpboat/contrib/systemd"
	"github.com/im-anuvaad/phpboat/internal/auth"
	"github.com/im-anuvaad/phpboat/internal/buildinfo"
	"github.com/im-anuvaad/phpboat/internal/edge"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/secrets"
	"github.com/im-anuvaad/phpboat/internal/store"
)

// runSetupHost is the second half of install.sh (docs/architecture.md):
// directories, master key, systemd unit, a new one-time password, start
// phpboat, print how to sign in. install.sh stops phpboat before it replaces
// the binary; running this again is safe.
func runSetupHost(stdout, stderr io.Writer) int {
	if err := setupHost(stdout); err != nil {
		fmt.Fprintln(stderr, "phpboat setup-host:", err)
		return 1
	}
	return 0
}

func setupHost(out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root")
	}
	if serviceActive() {
		return errors.New("phpboat is running; stop it first (systemctl stop phpboat)")
	}
	// phpboat is stopped, so anything on 443 is another program: phpboat
	// could not start, and that program would answer the readiness check
	// below in its place. Fail before changing anything.
	if err := portFree(":443"); err != nil {
		return err
	}
	lay := layout.Default()
	if err := layout.EnsureBase(lay); err != nil {
		return err
	}
	if _, err := secrets.LoadOrCreate(layout.MasterKey); err != nil {
		return err
	}
	if cur, err := os.ReadFile(systemd.UnitPath); err != nil || string(cur) != systemd.Unit {
		tmp := systemd.UnitPath + ".tmp"
		if err := os.WriteFile(tmp, []byte(systemd.Unit), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, systemd.UnitPath); err != nil {
			return err
		}
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--quiet", "phpboat"); err != nil {
		return err
	}

	// The one-time password is written while phpboat is stopped (it caches
	// the state in memory).
	ctx := context.Background()
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	pw, until, err := auth.NewSetupPassword(ctx, st)
	var domain string
	if v, ok, _ := st.GetSetting(ctx, edge.SettingDashboardDomain); ok {
		domain = string(v)
	}
	st.Close()
	if err != nil {
		return err
	}
	if err := os.Chmod(lay.DBPath(), 0o600); err != nil {
		return err
	}

	if err := systemctl("start", "phpboat"); err != nil {
		return err
	}
	ready := waitListening("127.0.0.1:443", 30*time.Second)

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ip, _ := edge.DetectIP(dctx, "tcp4")
	cancel()
	if ip == "" {
		ip = "<this server's IP address>"
	}

	if ready {
		fmt.Fprintf(out, "\nphpboat %s is running.\n\n", buildinfo.Version)
	} else {
		fmt.Fprintf(out, "\nphpboat %s was started but is not answering on port 443 yet; see: journalctl -u phpboat\n\n", buildinfo.Version)
	}
	fmt.Fprintf(out, "  Open:              https://%s\n", ip)
	fmt.Fprintf(out, "                     Your browser warns that the certificate is not trusted: expected here, continue.\n")
	fmt.Fprintf(out, "  One-time password: %s\n", pw)
	fmt.Fprintf(out, "                     Leave the email empty. Works once, until %s.\n", until.UTC().Format("2006-01-02 15:04 UTC"))
	if domain != "" {
		fmt.Fprintf(out, "\n  Dashboard:         https://%s\n", domain)
		fmt.Fprintf(out, "                     Sign in there as usual; the address above is only needed if that fails.\n")
	}
	fmt.Fprintln(out)
	return nil
}

// portFree reports another program listening on addr, naming it if ss can.
func portFree(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err == nil {
		return l.Close()
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	who := "another program"
	if out, err := exec.Command("ss", "-Hltnp", "sport = "+addr).Output(); err == nil {
		if _, rest, ok := strings.Cut(string(out), `(("`); ok {
			if name, _, ok := strings.Cut(rest, `"`); ok && name != "" {
				who = name
			}
		}
	}
	return fmt.Errorf("port %s is already in use by %s; phpboat serves HTTPS itself and needs it.\n"+
		"Stop and disable that program (e.g. systemctl disable --now %s), then run the install command again", strings.TrimPrefix(addr, ":"), who, who)
}

func waitListening(addr string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func serviceActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "phpboat").Run() == nil
}
