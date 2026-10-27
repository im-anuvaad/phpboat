// Package buildinfo exposes version metadata injected at build time via
// -ldflags "-X github.com/im-anuvaad/phpboat/internal/buildinfo.Version=...".
package buildinfo

import (
	"fmt"
	"runtime"
)

// Set at build time. Defaults apply to plain `go build` / `go run`.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a single human-readable version line.
func String() string {
	return fmt.Sprintf("phpboat %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
