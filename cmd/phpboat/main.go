// Command phpboat is a minimal single-binary PaaS for small PHP + SQLite web apps.
//
// It has no admin CLI: install.sh runs `phpboat setup-host`, systemd runs
// `phpboat serve`, and everything else happens in the dashboard
// (docs/architecture.md, D33).
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/im-anuvaad/phpboat/internal/buildinfo"
)

const usage = `phpboat - minimal PaaS for small PHP + SQLite web apps

Install or update it with:
  curl -fsSL https://github.com/im-anuvaad/phpboat/releases/latest/download/install.sh | sudo bash
and manage it in the dashboard. These commands are used by the installer
and systemd:

  version      Print version information
  setup-host   Prepare the host, start phpboat, print a one-time password (run by install.sh)
  serve        Run the phpboat service (run by systemd)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, buildinfo.String())
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve":
		if len(args) > 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return runServe(stderr)
	case "setup-host":
		if len(args) > 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return runSetupHost(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "phpboat: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
