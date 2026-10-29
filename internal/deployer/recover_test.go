package deployer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/store"
)

// A phpboat restart while a deploy switches builds (current.prev exists)
// puts current back to the build that was serving before.
func TestRecoverSwitch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "phpboat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	l := layout.Layout{Root: root, UID: os.Getuid(), GID: os.Getgid()}
	d, err := New(ctx, Deps{Store: db, Layout: l})
	if err != nil {
		t.Fatal(err)
	}
	const name = "web"
	for _, id := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		if err := os.MkdirAll(l.BuildDir(name, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name      string
		prev, cur string // prev "" = no previous build
		want      string // current after recovery ("" = none)
	}{
		{name: "switched to the new build", prev: "aaaaaaaaaaaa", cur: "bbbbbbbbbbbb", want: "aaaaaaaaaaaa"},
		{name: "not switched yet", prev: "aaaaaaaaaaaa", cur: "aaaaaaaaaaaa", want: "aaaaaaaaaaaa"},
		{name: "first deploy", prev: "", cur: "bbbbbbbbbbbb", want: ""},
		{name: "previous build gone", prev: "cccccccccccc", cur: "bbbbbbbbbbbb", want: "bbbbbbbbbbbb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := d.swapCurrent(name, c.cur); err != nil {
				t.Fatal(err)
			}
			if err := d.markSwitch(name, c.prev); err != nil {
				t.Fatal(err)
			}
			d.recoverSwitch(name)
			if got := d.currentBuild(name); got != c.want {
				t.Fatalf("current = %q, want %q", got, c.want)
			}
			if _, err := os.Lstat(l.PreviousLink(name)); !os.IsNotExist(err) {
				t.Fatalf("current.prev still there: %v", err)
			}
		})
	}
	// Without current.prev nothing changes.
	if err := d.swapCurrent(name, "bbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	d.recoverSwitch(name)
	if got := d.currentBuild(name); got != "bbbbbbbbbbbb" {
		t.Fatalf("no marker: current = %q", got)
	}
}
