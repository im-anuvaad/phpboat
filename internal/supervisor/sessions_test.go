package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanSessions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{"sess_old": 25 * time.Hour, "sess_new": time.Hour, "other_old": 48 * time.Hour} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	os.Mkdir(filepath.Join(dir, "sess_dir"), 0o700)
	if n := CleanSessions(dir, SessionMaxIdle, now); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	for name, want := range map[string]bool{"sess_old": false, "sess_new": true, "other_old": true, "sess_dir": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", name, err == nil, want)
		}
	}
	if n := CleanSessions(filepath.Join(dir, "missing"), SessionMaxIdle, now); n != 0 {
		t.Errorf("missing dir: %d", n)
	}
}
