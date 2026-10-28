package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im-anuvaad/phpboat/internal/apps"
	"github.com/im-anuvaad/phpboat/internal/auth"
	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/deployer"
	"github.com/im-anuvaad/phpboat/internal/edge"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/store"
	"github.com/im-anuvaad/phpboat/internal/supervisor"
)

const testPassword = "correct horse battery"

// apiServer is a dashboard with a real store, auth, app registry and
// deployer, one stored app, and no edge, metrics or backups.
func apiServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "phpboat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lay := layout.Layout{Root: root}
	sup := supervisor.New(supervisor.Deps{Layout: lay, Cgroups: &cgroup.Manager{}, Log: log})
	dep, err := deployer.New(ctx, deployer.Deps{Store: st, Layout: lay, Supervisor: sup, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Writer().Exec(`INSERT INTO apps (name, repo, branch, domain, memory_max, cpu_max, pids_max, created_at, concurrency)
		VALUES ('web', 'https://github.com/me/web', 'main', 'web.example.com', 268435456, 1, 256, 0, 4)`); err != nil {
		t.Fatal(err)
	}
	a := auth.New(st)
	if err := a.SetAdmin(ctx, "me@example.com", testPassword); err != nil {
		t.Fatal(err)
	}
	s := &Server{Auth: a, Apps: apps.New(st, nil, dep, sup, nil, func() string { return "" }, log),
		Dep: dep, Sup: sup, Store: st, Layout: lay, Version: "v9.9.9", Log: log}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	session, err := a.Login(ctx, "me@example.com", testPassword, "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	return s, h, session
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func apiGet(path, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://dash.example.com"+path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

// createToken goes through the Account page like a user: the token is in
// that one response and nowhere else.
func createToken(t *testing.T, s *Server, h http.Handler, session string) string {
	t.Helper()
	sess, err := s.Auth.Session(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {sess.CSRF}, "current": {testPassword}}
	r := httptest.NewRequest(http.MethodPost, "https://dash.example.com/account/api-token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://dash.example.com")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	w := do(h, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create token: %d %s", w.Code, w.Body)
	}
	i := strings.Index(w.Body.String(), auth.APITokenPrefix)
	if i < 0 {
		t.Fatal("token not shown after creating it")
	}
	tok := w.Body.String()[i:]
	tok = tok[:strings.IndexByte(tok, '"')]
	for _, c := range w.Result().Cookies() {
		if strings.Contains(c.Value, tok) {
			t.Fatal("token put in a cookie")
		}
	}
	// Shown once only.
	r = httptest.NewRequest(http.MethodGet, "https://dash.example.com/account", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	if w := do(h, r); w.Code != http.StatusOK || strings.Contains(w.Body.String(), tok) || !strings.Contains(w.Body.String(), "Rotate token") {
		t.Fatalf("account page after creating: %d, shows token again: %v", w.Code, strings.Contains(w.Body.String(), tok))
	}
	return tok
}

func TestAPIAuth(t *testing.T) {
	s, h, session := apiServer(t)

	// Without a token (none exists yet) everything is a JSON 401.
	w := do(h, apiGet("/api/v1/status", ""))
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("no token: %d %v", w.Code, w.Header())
	}

	// A dashboard session cookie is not enough: the API never acts for a browser.
	r := apiGet("/api/v1/status", "")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	if w := do(h, r); w.Code != http.StatusUnauthorized {
		t.Fatalf("session cookie accepted by the API: %d", w.Code)
	}

	tok := createToken(t, s, h, session)
	if w := do(h, apiGet("/api/v1/status", tok)); w.Code != http.StatusOK {
		t.Fatalf("valid token: %d %s", w.Code, w.Body)
	}
	if w := do(h, apiGet("/api/v1/status", tok+"x")); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", w.Code)
	}

	// Not on the setup address, even with the right token.
	r = apiGet("/api/v1/status", tok)
	r = r.WithContext(edge.MarkDirect(r.Context()))
	if w := do(h, r); w.Code != http.StatusForbidden {
		t.Fatalf("setup address: %d", w.Code)
	}

	// Read-only, and unknown paths are JSON 404s.
	r = httptest.NewRequest(http.MethodPost, "https://dash.example.com/api/v1/status", nil)
	r.Header.Set("Origin", "https://dash.example.com")
	r.Header.Set("Authorization", "Bearer "+tok)
	if w := do(h, r); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d %s", w.Code, w.Body)
	}
	if w := do(h, apiGet("/api/v1/nothing", tok)); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("unknown path: %d %s", w.Code, w.Body)
	}

	// Rotating from the Account page revokes the previous token.
	tok2 := createToken(t, s, h, session)
	if w := do(h, apiGet("/api/v1/status", tok)); w.Code != http.StatusUnauthorized {
		t.Fatalf("old token after rotation: %d", w.Code)
	}
	if w := do(h, apiGet("/api/v1/status", tok2)); w.Code != http.StatusOK {
		t.Fatalf("new token: %d", w.Code)
	}
}

func TestAPIResponses(t *testing.T) {
	s, h, session := apiServer(t)
	tok := createToken(t, s, h, session)

	var st struct {
		OK      bool   `json:"ok"`
		Version string `json:"phpboat_version"`
		Time    string `json:"time"`
		Host    struct {
			CPUs int `json:"cpus"`
		} `json:"host"`
		Summary  struct{ Apps, Healthy, Unhealthy int } `json:"summary"`
		Warnings []string                               `json:"warnings"`
		Apps     []map[string]any                       `json:"apps"`
	}
	w := do(h, apiGet("/api/v1/status", tok))
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if _, err := time.Parse(time.RFC3339, st.Time); err != nil || st.Version != "v9.9.9" || st.Host.CPUs < 1 {
		t.Errorf("status: %+v", st)
	}
	// The stored app was never deployed, so it is not healthy and ok is false.
	if st.OK || st.Summary.Apps != 1 || st.Summary.Unhealthy != 1 || len(st.Apps) != 1 || st.Warnings == nil {
		t.Errorf("summary: %+v", st)
	}
	app := st.Apps[0]
	if app["name"] != "web" || app["url"] != "https://web.example.com/" || app["healthy"] != false || app["last_backup_at"] != nil {
		t.Errorf("app summary: %v", app)
	}

	w = do(h, apiGet("/api/v1/apps", tok))
	var list struct{ Apps []map[string]any }
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Apps) != 1 {
		t.Errorf("apps: %s", w.Body)
	}

	w = do(h, apiGet("/api/v1/apps/web", tok))
	var d map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil || w.Code != http.StatusOK {
		t.Fatalf("app: %d %s", w.Code, w.Body)
	}
	limits, _ := d["limits"].(map[string]any)
	if d["name"] != "web" || d["repo"] != "https://github.com/me/web" || limits["memory_bytes"] != float64(256<<20) ||
		d["now"] != nil || d["last_deployment"] != nil || !strings.Contains(w.Body.String(), "never deployed") {
		t.Errorf("app detail: %s", w.Body)
	}
	if w := do(h, apiGet("/api/v1/apps/nope", tok)); w.Code != http.StatusNotFound {
		t.Errorf("unknown app: %d", w.Code)
	}
}

func TestAPIJSONValues(t *testing.T) {
	b, _ := json.Marshal(struct {
		A, B apiFloat
		T, Z apiTime
	}{apiFloat(1.23456), apiFloat(nan()), apiTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))), apiTime{}})
	if want := `{"A":1.23,"B":null,"T":"2026-01-02T02:04:05Z","Z":null}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func nan() float64 { z := 0.0; return z / z }
