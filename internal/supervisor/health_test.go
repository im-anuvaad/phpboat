package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-anuvaad/phpboat/internal/app"
)

type fakeSite struct {
	codes map[string]int
	delay map[string]time.Duration
	calls atomic.Int32
}

func (f *fakeSite) Ping(ctx context.Context, host, path string) (int, error) {
	f.calls.Add(1)
	if d := f.delay[path]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if c, ok := f.codes[path]; ok {
		return c, nil
	}
	return 0, errors.New("no response")
}

// The error names the real problem, not a probe cut short by the deadline.
func TestHealthCheckErrors(t *testing.T) {
	p := Policy{HealthTimeout: 400 * time.Millisecond, HealthInterval: 30 * time.Millisecond}
	sock := filepath.Join(t.TempDir(), "php.sock")
	site := &fakeSite{
		codes: map[string]int{"/healthz": 200, "/redirect": 302, "/wrong": 404, "/slow": 404, "/busy": 503, "/broken": 502},
		delay: map[string]time.Duration{"/slow": 150 * time.Millisecond},
	}
	check := func(path string) error {
		return healthCheck(context.Background(), site, sock, app.Spec{Name: "web", HealthPath: path}, p)
	}
	if err := check("/healthz"); err == nil || !strings.Contains(err.Error(), "has not opened its socket") {
		t.Fatalf("no socket: %v", err)
	}
	os.WriteFile(sock, nil, 0o600)
	for path, want := range map[string]string{"/wrong": "returned 404", "/slow": "returned 404", "/busy": "not accepting connections", "/broken": "failed the request"} {
		if err := check(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
	for _, path := range []string{"/healthz", "/redirect"} {
		if err := check(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
