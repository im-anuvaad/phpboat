package supervisor

import (
	"slices"
	"testing"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/layout"
)

func TestEnviron(t *testing.T) {
	a := &App{dirs: layout.Layout{Root: "/r"}}
	spec := app.Spec{Name: "web", Port: 20001, Limits: app.Limits{MemoryMax: 256 << 20}}

	env := a.environ(spec)
	for _, want := range []string{"PHPBOAT_CONTRACT=3", "DATA_DIR=/r/apps/web/data", "PORT=20001"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s in %v", want, env)
		}
	}
	for _, kv := range env {
		if len(kv) > 11 && kv[:11] == "GOMEMLIMIT=" {
			t.Errorf("apps are PHP, no GOMEMLIMIT expected: %s", kv)
		}
	}
}
