package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitsBranchesAndTarball(t *testing.T) {
	// The "storage" host serves signed archive URLs; it must never see the token.
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token forwarded to the storage host")
		}
		w.Write([]byte("tarball bytes"))
	}))
	defer storage.Close()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		switch r.URL.Path {
		case "/repos/me/app/commits":
			if r.URL.Query().Get("sha") != "feature/x" {
				t.Errorf("branch = %q", r.URL.Query().Get("sha"))
			}
			w.Write([]byte(`[{"sha":"` + sha + `","commit":{"message":"Fix it\n\nlonger body","author":{"name":"Ann","date":"2026-10-01T10:00:00Z"}}}]`))
		case "/repos/me/app/branches/feature/x":
			w.Write([]byte(`{"name":"feature/x","commit":{"sha":"` + sha + `"}}`))
		case "/repos/me/app/tarball/" + sha:
			http.Redirect(w, r, storage.URL+"/signed/"+sha, http.StatusFound)
		case "/repos/me/app":
			w.Write([]byte(`{"full_name":"me/app","private":true,"default_branch":"main"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	ctx := context.Background()
	a := &API{Token: "tok", Base: api.URL}
	repo := Repo{Owner: "me", Name: "app"}
	cs, err := a.Commits(ctx, repo, "feature/x", 10)
	if err != nil || len(cs) != 1 || cs[0].SHA != sha || cs[0].Subject != "Fix it" || cs[0].Author != "Ann" {
		t.Fatalf("commits: %+v %v", cs, err)
	}
	info, err := a.Repository(ctx, repo)
	if err != nil || info.DefaultBranch != "main" {
		t.Fatalf("repository: %+v %v", info, err)
	}
	head, err := a.BranchHead(ctx, repo, "feature/x")
	if err != nil || head != sha {
		t.Fatalf("branch head: %q %v", head, err)
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "source.tar.gz")
	if err := a.Tarball(ctx, repo, sha, p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "tarball bytes" {
		t.Fatalf("got %q", b)
	}
	if err := a.Tarball(ctx, repo, sha, filepath.Join(dir, "small"), 4); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("size limit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "small")); err == nil {
		t.Fatal("a too-large download must not be left behind")
	}

	// Without the token the private repository looks missing.
	anon := &API{Base: api.URL}
	if _, err := anon.Commits(ctx, repo, "main", 10); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "GitHub token") {
		t.Fatalf("private repo without token: %v", err)
	}
}
