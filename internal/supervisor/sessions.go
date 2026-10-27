package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionMaxIdle is how long an unused PHP session file is kept. PHP's own
// garbage collection is off (Debian's default, and phpboat's pool sets it
// too), so phpboat removes old files itself; 24 h is generous next to PHP's
// default of 24 minutes, so an app's longer session.gc_maxlifetime survives.
const SessionMaxIdle = 24 * time.Hour

// CleanSessions removes sess_* files in dir not modified for maxIdle. It
// returns how many it removed.
func CleanSessions(dir string, maxIdle time.Duration, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "sess_") || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < maxIdle {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// CleanAllSessions runs CleanSessions for every app.
func (s *Supervisor) CleanAllSessions(now time.Time) {
	for _, a := range s.Apps() {
		name := a.Spec().Name
		if n := CleanSessions(s.deps.Layout.SessionsDir(name), SessionMaxIdle, now); n > 0 {
			s.deps.Log.Info("removed old PHP sessions", "app", name, "count", n)
		}
	}
}
