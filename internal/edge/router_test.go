package edge

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterToApps(t *testing.T) {
	state := map[string]Availability{"blog": Available, "shop": Deploying, "wiki": Available, "down": NotRunning}
	rt := &Router{
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		State: func(app string) Availability { return state[app] },
		Handler: func(app string) http.Handler {
			if app == "wiki" {
				return nil // available but no handler yet
			}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/missing" {
					ErrorPage(w, r, http.StatusNotFound)
					return
				}
				io.WriteString(w, "hello from "+app)
			})
		},
		Dashboard: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "dashboard") }),
	}
	rt.SetDashboardHost("dash.example.com")
	rt.SetRoutes([]Route{{Host: "blog.example.com", App: "blog"}, {Host: "shop.example.com", App: "shop"},
		{Host: "wiki.example.com", App: "wiki"}, {Host: "down.example.com", App: "down"}})
	cases := []struct {
		host, path string
		code       int
		body       string
	}{
		{"blog.example.com", "/", 200, "hello from blog"},
		{"BLOG.example.com:443", "/", 200, "hello from blog"},
		{"blog.example.com", "/missing", 404, "Not found"},
		{"shop.example.com", "/", 503, "Deploying"},
		{"wiki.example.com", "/", 503, "Starting"},
		{"down.example.com", "/", 503, "not running"},
		{"dash.example.com", "/", 200, "dashboard"},
		{"other.example.com", "/", 404, "No app is configured"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://"+c.host+c.path, nil)
		r.Host = c.host
		r.RemoteAddr = "173.245.48.10:1234" // not a setup connection
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s%s: %d %q, want %d %q", c.host, c.path, w.Code, w.Body.String(), c.code, c.body)
		}
	}
	st := rt.Stats()["blog"]
	if st.Requests != 3 || st.Status[2] != 2 || st.Status[4] != 1 {
		t.Errorf("blog stats %+v", st)
	}
	if st := rt.Stats()["shop"]; st.Status[5] != 1 {
		t.Errorf("shop stats %+v", st)
	}
}
