package supervisor

import (
	"slices"
	"strings"
	"testing"

	"github.com/im-anuvaad/phpboat/internal/app"
	"github.com/im-anuvaad/phpboat/internal/layout"
)

func TestEnviron(t *testing.T) {
	a := &App{dirs: layout.Layout{Root: "/r"}}
	spec := app.Spec{Name: "web", Domain: "web.example.com", BuildID: "0123456789ab", CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		Env: map[string]string{"SECRET": "s"}, Limits: app.Limits{MemoryMax: 256 << 20}}

	env := a.environ(spec)
	for _, want := range []string{"PHPBOAT_CONTRACT=1", "DATA_DIR=/r/apps/web/data", "TMPDIR=/r/apps/web/tmp", "PHPBOAT_APP=web",
		"PHPBOAT_DOMAIN=web.example.com", "PHPBOAT_RELEASE=0123456789ab", "PHPBOAT_COMMIT=0123456789abcdef0123456789abcdef01234567", "SECRET=s", "LANG=C.UTF-8"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s in %v", want, env)
		}
	}
	for _, kv := range env {
		for _, gone := range []string{"PORT=", "HOST=", "GOMEMLIMIT=", "HOME="} {
			if strings.HasPrefix(kv, gone) {
				t.Errorf("unexpected %s", kv)
			}
		}
	}
}
