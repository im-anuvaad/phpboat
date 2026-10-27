package phpserve

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/im-anuvaad/phpboat/internal/fastcgi"
	"github.com/im-anuvaad/phpboat/internal/fastcgi/fcgitest"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// echoFPM answers every request with its params as JSON (+ the body).
func echoFPM(t *testing.T) *fcgitest.Server {
	return fcgitest.New(t, func(r *fcgitest.Request) fcgitest.Response {
		r.Params["_STDIN"] = string(r.Stdin)
		b, _ := json.Marshal(r.Params)
		return fcgitest.Response{Header: map[string]string{"Content-Type": "application/json", "X-Php": "1"}, Body: string(b), Stderr: "PHP Notice: hi\n"}
	})
}

type fixture struct {
	root, build, data string
	fpm               *fcgitest.Server
	logMu             sync.Mutex
	logs              []string
}

func (f *fixture) site(t *testing.T, docroot string) *Site {
	t.Helper()
	s, err := New(Config{
		Build: f.build, Docroot: docroot, DataDir: f.data,
		FastCGI:  &fastcgi.Client{Address: f.fpm.Socket},
		ClientIP: func(r *http.Request) string { return "203.0.113.7" },
		PHPLog: func(r *http.Request, line string) {
			f.logMu.Lock()
			f.logs = append(f.logs, r.Method+" "+r.URL.Path+": "+line)
			f.logMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	f := &fixture{root: root, build: filepath.Join(root, "build"), data: filepath.Join(root, "data"), fpm: echoFPM(t)}
	b := f.build
	for p, c := range map[string]string{
		"public/index.php":       "<?php",
		"public/about.php":       "<?php",
		"public/blog/index.php":  "<?php",
		"public/docs/index.html": "<h1>docs</h1>",
		"public/style.css":       "body{}",
		"public/img/dot.svg":     "<svg/>",
		"public/data.unknownext": "raw",
		"public/.env":            "SECRET=1",
		"public/.git/config":     "[core]",
		"public/inc/config.inc":  "<?php $pw='x';",
		"public/old.phtml":       "<?php",
		"public/dir.php/x.txt":   "a folder named dir.php",
		"notes.sql":              "-- private",
		"composer.json":          "{}",
		"composer.lock":          "{}",
		"phpboat.toml":           "contract = 1",
		"vendor/autoload.php":    "<?php",
		"index.php":              "<?php",
		"page.php":               "<?php",
		"db.sqlite":              "SQLite format 3",
		"assets/app.log":         "log",
		"assets/site.css":        "body{}",
		"lib/Helpers.php":        "<?php",
	} {
		write(t, filepath.Join(b, p), c)
	}
	// A persist folder: the build has a symlink into DATA_DIR.
	for p, c := range map[string]string{
		"public/uploads/a.txt":     "uploaded",
		"public/uploads/shell.php": "<?php system($_GET['c']);",
		"public/uploads/SHELL.PHP": "<?php system($_GET['c']);",
		"public/uploads/x.html":    "<script>alert(1)</script>",
	} {
		write(t, filepath.Join(f.data, p), c)
	}
	if err := os.Symlink(filepath.Join(f.data, "public/uploads"), filepath.Join(b, "public/uploads")); err != nil {
		t.Fatal(err)
	}
	// A symlink escaping the build.
	write(t, filepath.Join(root, "outside/secret.txt"), "outside")
	write(t, filepath.Join(root, "outside/evil.php"), "<?php")
	os.Symlink(filepath.Join(root, "outside/secret.txt"), filepath.Join(b, "public/link.txt"))
	os.Symlink(filepath.Join(root, "outside/evil.php"), filepath.Join(b, "public/evil.php"))
	return f
}

type result struct {
	code   int
	body   string
	header http.Header
	params map[string]string // set when PHP ran
}

func do(t *testing.T, s *Site, method, target string, hdr map[string]string, body io.Reader) result {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	r.Host = "blog.example.com"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	res := result{code: w.Code, body: w.Body.String(), header: w.Header()}
	if w.Header().Get("X-Php") == "1" {
		json.Unmarshal(w.Body.Bytes(), &res.params)
	}
	return res
}

func TestRouting(t *testing.T) {
	f := newFixture(t)
	pub := f.site(t, "public")
	root := f.site(t, ".")
	cases := []struct {
		name   string
		s      *Site
		path   string
		code   int
		script string // PHP script expected to run ("" = not PHP)
		pi     string // PATH_INFO
		body   string // substring of a static body
	}{
		{"home", pub, "/", 200, "/index.php", "", ""},
		{"pretty URL → front controller", pub, "/posts/42", 200, "/index.php", "", ""},
		{"script", pub, "/about.php", 200, "/about.php", "", ""},
		{"script + PATH_INFO", pub, "/about.php/x/y", 200, "/about.php", "/x/y", ""},
		{"folder index.php", pub, "/blog/", 200, "/blog/index.php", "", ""},
		{"folder index.html", pub, "/docs/", 200, "", "", "<h1>docs</h1>"},
		{"folder without slash", pub, "/blog", 301, "", "", ""},
		{"static css", pub, "/style.css", 200, "", "", "body{}"},
		{"static svg", pub, "/img/dot.svg", 200, "", "", "<svg/>"},
		{"missing .php → front controller", pub, "/missing.php", 200, "/index.php", "", ""},
		{"folder named x.php is not a script", pub, "/dir.php/x.txt", 200, "", "", "a folder named"},
		{"dotfile", pub, "/.env", 404, "", "", ""},
		{"dot folder", pub, "/.git/config", 404, "", "", ""},
		{"encoded dotfile", pub, "/%2eenv", 404, "", "", ""},
		{".inc never served", pub, "/inc/config.inc", 404, "", "", ""},
		{".phtml never served or run", pub, "/old.phtml", 404, "", "", ""},
		{"traversal is cleaned", pub, "/../notes.sql", 200, "/index.php", "", ""},
		{"symlink out of the build: static", pub, "/link.txt", 404, "", "", ""},
		{"symlink out of the build: php", pub, "/evil.php", 404, "", "", ""},
		{"upload served", pub, "/uploads/a.txt", 200, "", "", "uploaded"},
		{"uploaded .php never runs", pub, "/uploads/shell.php", 404, "", "", ""},
		{"uploaded .php via PATH_INFO", pub, "/uploads/shell.php/x", 404, "", "", ""},
		{"uploaded .PHP never served", pub, "/uploads/SHELL.PHP", 404, "", "", ""},
		{"root: index", root, "/", 200, "/index.php", "", ""},
		{"root: page", root, "/page.php", 200, "/page.php", "", ""},
		{"root: lib file runs (the user's choice)", root, "/lib/Helpers.php", 200, "/lib/Helpers.php", "", ""},
		{"root: static", root, "/assets/site.css", 200, "", "", "body{}"},
		{"root: vendor denied", root, "/vendor/autoload.php", 404, "", "", ""},
		{"root: composer.json denied", root, "/composer.json", 404, "", "", ""},
		{"root: composer.lock denied", root, "/composer.lock", 404, "", "", ""},
		{"root: phpboat.toml denied", root, "/phpboat.toml", 404, "", "", ""},
		{"root: *.sql denied", root, "/notes.sql", 404, "", "", ""},
		{"root: *.sqlite denied", root, "/db.sqlite", 404, "", "", ""},
		{"root: *.log denied", root, "/assets/app.log", 404, "", "", ""},
		{"root: case-insensitive ext", root, "/NOTES.SQL", 404, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := do(t, tc.s, "GET", tc.path, nil, nil)
			if res.code != tc.code {
				t.Fatalf("code %d, want %d (body %.80q)", res.code, tc.code, res.body)
			}
			if got := res.params["SCRIPT_NAME"]; got != tc.script {
				t.Errorf("script %q, want %q", got, tc.script)
			}
			if tc.script != "" {
				if got := res.params["PATH_INFO"]; got != tc.pi {
					t.Errorf("PATH_INFO %q, want %q", got, tc.pi)
				}
				if want := filepath.Join(tc.s.docroot, tc.script); res.params["SCRIPT_FILENAME"] != want {
					t.Errorf("SCRIPT_FILENAME %q, want %q", res.params["SCRIPT_FILENAME"], want)
				}
			}
			if tc.body != "" && !strings.Contains(res.body, tc.body) {
				t.Errorf("body %q lacks %q", res.body, tc.body)
			}
		})
	}
}

func TestRedirectKeepsQuery(t *testing.T) {
	f := newFixture(t)
	res := do(t, f.site(t, "public"), "GET", "/blog?page=2", nil, nil)
	if res.code != 301 || res.header.Get("Location") != "/blog/?page=2" {
		t.Errorf("%d %q", res.code, res.header.Get("Location"))
	}
}

func TestParams(t *testing.T) {
	f := newFixture(t)
	s := f.site(t, "public")
	res := do(t, s, "POST", "/about.php/p?q=1&r=%20", map[string]string{
		"Content-Type":      "application/x-www-form-urlencoded",
		"X-Forwarded-For":   "6.6.6.6",
		"X-Forwarded-Proto": "http",
		"X-Real-IP":         "6.6.6.6",
		"Forwarded":         "for=6.6.6.6",
		"Proxy":             "http://evil:8080",
		"X_Forwarded_For":   "6.6.6.6",
		"Accept-Language":   "de",
		"Cookie":            "a=1",
	}, strings.NewReader("a=1&b=2"))
	if res.code != 200 {
		t.Fatalf("code %d", res.code)
	}
	p := res.params
	want := map[string]string{
		"REQUEST_METHOD": "POST", "REQUEST_URI": "/about.php/p?q=1&r=%20", "QUERY_STRING": "q=1&r=%20",
		"SCRIPT_NAME": "/about.php", "PATH_INFO": "/p", "DOCUMENT_URI": "/about.php/p",
		"HTTPS": "on", "REQUEST_SCHEME": "https", "SERVER_PORT": "443", "SERVER_NAME": "blog.example.com",
		"HTTP_HOST": "blog.example.com", "REMOTE_ADDR": "203.0.113.7",
		"CONTENT_TYPE": "application/x-www-form-urlencoded", "CONTENT_LENGTH": "7", "_STDIN": "a=1&b=2",
		"HTTP_ACCEPT_LANGUAGE": "de", "HTTP_COOKIE": "a=1", "DOCUMENT_ROOT": filepath.Join(f.build, "public"),
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %q, want %q", k, p[k], v)
		}
	}
	for _, k := range []string{"HTTP_X_FORWARDED_FOR", "HTTP_X_FORWARDED_PROTO", "HTTP_X_REAL_IP", "HTTP_FORWARDED", "HTTP_PROXY", "HTTP_CONTENT_TYPE", "HTTP_CONTENT_LENGTH"} {
		if v, ok := p[k]; ok {
			t.Errorf("%s must not be passed (got %q)", k, v)
		}
	}
	f.logMu.Lock()
	defer f.logMu.Unlock()
	if len(f.logs) != 1 || f.logs[0] != "POST /about.php/p: PHP Notice: hi" {
		t.Errorf("logs %q", f.logs)
	}
}

func TestChunkedBodyIsCounted(t *testing.T) {
	f := newFixture(t)
	s := f.site(t, "public")
	r := httptest.NewRequest("POST", "/about.php", io.MultiReader(strings.NewReader("chunk1"), strings.NewReader("chunk2")))
	r.ContentLength = -1
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var p map[string]string
	json.Unmarshal(w.Body.Bytes(), &p)
	if p["CONTENT_LENGTH"] != "12" || p["_STDIN"] != "chunk1chunk2" {
		t.Errorf("%q %q", p["CONTENT_LENGTH"], p["_STDIN"])
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t)
	s := f.site(t, "public")
	if res := do(t, s, "GET", "/", map[string]string{"Cookie": strings.Repeat("a", MaxHeaderBytes)}, nil); res.code != 431 {
		t.Errorf("big headers: %d", res.code)
	}
	r := httptest.NewRequest("POST", "/about.php", strings.NewReader("x"))
	r.ContentLength = MaxBodyBytes + 1
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Errorf("big body: %d", w.Code)
	}
	if res := do(t, s, "POST", "/style.css", nil, nil); res.code != 405 || res.header.Get("Allow") == "" {
		t.Errorf("POST to static: %d", res.code)
	}
}

func TestStaticHeaders(t *testing.T) {
	f := newFixture(t)
	s := f.site(t, "public")
	res := do(t, s, "GET", "/style.css", nil, nil)
	if res.header.Get("Content-Type") != "text/css; charset=utf-8" || res.header.Get("X-Content-Type-Options") != "nosniff" || res.header.Get("ETag") == "" || res.header.Get("Last-Modified") == "" {
		t.Errorf("headers %v", res.header)
	}
	if res.header.Get("Content-Security-Policy") != "" {
		t.Error("build files get no sandbox CSP")
	}
	if res := do(t, s, "GET", "/data.unknownext", nil, nil); res.header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("unknown type: %q", res.header.Get("Content-Type"))
	}
	up := do(t, s, "GET", "/uploads/x.html", nil, nil)
	if !strings.HasPrefix(up.header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("uploaded html without sandbox CSP: %v", up.header)
	}
	etag := res.header.Get("ETag")
	if res := do(t, s, "GET", "/style.css", map[string]string{"If-None-Match": etag}, nil); res.code != 304 {
		t.Errorf("If-None-Match: %d", res.code)
	}
	if res := do(t, s, "GET", "/style.css", map[string]string{"Range": "bytes=0-3"}, nil); res.code != 206 || res.body != "body" {
		t.Errorf("Range: %d %q", res.code, res.body)
	}
	if res := do(t, s, "HEAD", "/style.css", nil, nil); res.code != 200 || res.body != "" {
		t.Errorf("HEAD: %d %q", res.code, res.body)
	}
}

func TestResponseHeadersFromPHP(t *testing.T) {
	f := newFixture(t)
	f.fpm = fcgitest.New(t, func(*fcgitest.Request) fcgitest.Response {
		return fcgitest.Response{Raw: "Status: 418 I'm a teapot\r\nSet-Cookie: a=1\r\nSet-Cookie: b=2\r\nConnection: close\r\nTransfer-Encoding: chunked\r\nContent-Type: text/plain\r\n\r\nshort and stout"}
	})
	res := do(t, f.site(t, "public"), "GET", "/", nil, nil)
	if res.code != 418 || res.body != "short and stout" || len(res.header.Values("Set-Cookie")) != 2 {
		t.Errorf("%d %q %v", res.code, res.body, res.header)
	}
	if res.header.Get("Connection") != "" || res.header.Get("Transfer-Encoding") != "" || res.header.Get("Status") != "" {
		t.Errorf("hop-by-hop headers passed: %v", res.header)
	}
}

func TestPoolDown(t *testing.T) {
	f := newFixture(t)
	s, err := New(Config{Build: f.build, Docroot: "public", FastCGI: &fastcgi.Client{Address: filepath.Join(f.root, "none.sock")}})
	if err != nil {
		t.Fatal(err)
	}
	res := do(t, s, "GET", "/", nil, nil)
	if res.code != 503 || res.header.Get("Retry-After") == "" {
		t.Errorf("%d %v", res.code, res.header)
	}
	// Static files still work while PHP is down.
	if res := do(t, s, "GET", "/style.css", nil, nil); res.code != 200 {
		t.Errorf("static while down: %d", res.code)
	}
}

func TestPing(t *testing.T) {
	f := newFixture(t)
	code, err := f.site(t, "public").Ping(t.Context(), "blog.example.com", "/healthz")
	if err != nil || code != 200 {
		t.Errorf("%d %v", code, err)
	}
	if p := f.fpm.Last().Params; p["REQUEST_URI"] != "/healthz" || p["SCRIPT_NAME"] != "/index.php" {
		t.Errorf("%v", p)
	}
}

func TestNewRejectsBadDocroots(t *testing.T) {
	f := newFixture(t)
	for _, d := range []string{"..", "../x", "/etc", "missing"} {
		if _, err := New(Config{Build: f.build, Docroot: d, FastCGI: &fastcgi.Client{}}); err == nil {
			t.Errorf("docroot %q accepted", d)
		}
	}
	os.Symlink("/etc", filepath.Join(f.build, "etc-link"))
	if _, err := New(Config{Build: f.build, Docroot: "etc-link", FastCGI: &fastcgi.Client{}}); err == nil {
		t.Error("docroot symlink out of the build accepted")
	}
}
