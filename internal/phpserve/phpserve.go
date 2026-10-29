// Package phpserve serves one PHP app (docs/plan.md §4): static files from
// its docroot directly, PHP scripts through FastCGI, a front controller as
// the fallback. PHP never runs from outside the deployed build (P14).
package phpserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/im-anuvaad/phpboat/internal/fastcgi"
	"github.com/im-anuvaad/phpboat/internal/phpfile"
)

// Limits (plan §4).
const (
	MaxHeaderBytes = 32 << 10  // Cloudflare's own request header limit; above → 431
	MaxBodyBytes   = 100 << 20 // Cloudflare's free-plan body limit; above → 413
)

// Config describes one deployed app.
type Config struct {
	// Build is the deployed build directory (read-only, root-owned).
	Build string
	// Docroot is the public folder relative to Build: "public" or ".".
	Docroot string
	// DataDir is the app's DATA_DIR. Persist folders are symlinks from the
	// build into it: their files are served, but never run as PHP.
	DataDir string
	// FastCGI reaches the app's FPM pool.
	FastCGI *fastcgi.Client
	// ClientIP returns the visitor's IP (edge.ClientIP). Default: the TCP peer.
	ClientIP func(*http.Request) string
	// PHPLog receives PHP's log lines (FastCGI stderr) with the request.
	PHPLog func(r *http.Request, line string)
	// Error writes an error response (the router's branded pages).
	// Default: http.Error.
	Error func(w http.ResponseWriter, r *http.Request, code int)
	// SpoolDir holds request bodies of unknown length while they are
	// counted (FastCGI needs CONTENT_LENGTH). Default: os.TempDir().
	SpoolDir string
}

// Site is an http.Handler for one app.
type Site struct {
	cfg       Config
	buildReal string
	dataReal  string
	docroot   string // absolute, as PHP sees it (SCRIPT_FILENAME, DOCUMENT_ROOT)
	rootMode  bool   // docroot "." → the repo root is public: extra denylist
}

// New checks cfg and returns the handler.
func New(cfg Config) (*Site, error) {
	if cfg.FastCGI == nil {
		return nil, errors.New("phpserve: no FastCGI client")
	}
	if cfg.Docroot == "" {
		cfg.Docroot = "public"
	}
	dr := filepath.Clean(cfg.Docroot)
	if filepath.IsAbs(dr) || dr == ".." || strings.HasPrefix(dr, "../") {
		return nil, fmt.Errorf("phpserve: docroot %q must stay inside the build", cfg.Docroot)
	}
	br, err := filepath.EvalSymlinks(cfg.Build)
	if err != nil {
		return nil, fmt.Errorf("phpserve: build: %w", err)
	}
	s := &Site{cfg: cfg, buildReal: br, docroot: filepath.Join(cfg.Build, dr), rootMode: dr == "."}
	if real, err := filepath.EvalSymlinks(s.docroot); err != nil {
		return nil, fmt.Errorf("phpserve: docroot %q: %w", cfg.Docroot, err)
	} else if !within(real, br) {
		return nil, fmt.Errorf("phpserve: docroot %q leaves the build", cfg.Docroot)
	}
	if cfg.DataDir != "" {
		if s.dataReal, err = filepath.EvalSymlinks(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("phpserve: data dir: %w", err)
		}
	}
	if s.cfg.ClientIP == nil {
		s.cfg.ClientIP = func(r *http.Request) string {
			h, _, _ := net.SplitHostPort(r.RemoteAddr)
			return h
		}
	}
	if s.cfg.Error == nil {
		s.cfg.Error = func(w http.ResponseWriter, _ *http.Request, code int) {
			http.Error(w, strconv.Itoa(code)+" "+http.StatusText(code), code)
		}
	}
	return s, nil
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// phpLike: extensions whose source must never be sent as a file.
var phpLike = phpfile.Like

var rootDenyNames = map[string]bool{"composer.json": true, "composer.lock": true, "phpboat.toml": true}
var rootDenyExt = map[string]bool{".sqlite": true, ".sqlite3": true, ".db": true, ".sql": true, ".log": true}

// denied: dotfiles anywhere; with docroot ".", also the repo's own files.
func (s *Site) denied(segs []string) bool {
	for _, seg := range segs {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	if s.rootMode && len(segs) > 0 {
		if segs[0] == "vendor" {
			return true
		}
		for _, seg := range segs {
			if rootDenyNames[seg] || rootDenyExt[strings.ToLower(filepath.Ext(seg))] {
				return true
			}
		}
	}
	return false
}

func headerBytes(r *http.Request) int {
	n := len(r.Method) + len(r.RequestURI) + len(r.Host)
	for k, vs := range r.Header {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}

// ServeHTTP implements http.Handler.
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if headerBytes(r) > MaxHeaderBytes {
		s.cfg.Error(w, r, http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	if strings.ContainsRune(r.URL.Path, 0) {
		s.cfg.Error(w, r, http.StatusBadRequest)
		return
	}
	upath := path.Clean("/" + r.URL.Path)
	var segs []string
	if upath != "/" {
		segs = strings.Split(upath[1:], "/")
	}
	if s.denied(segs) {
		s.cfg.Error(w, r, http.StatusNotFound)
		return
	}

	// 1. /a/b.php/extra → script /a/b.php, PATH_INFO /extra.
	prefix := ""
	for i, seg := range segs {
		prefix += "/" + seg
		if strings.HasSuffix(seg, ".php") && isFile(filepath.Join(s.docroot, prefix)) {
			s.php(w, r, prefix, upath, strings.Join(append([]string{""}, segs[i+1:]...), "/"))
			return
		}
	}

	fp := filepath.Join(s.docroot, upath)
	if st, err := os.Stat(fp); err == nil {
		switch {
		case st.Mode().IsRegular():
			s.static(w, r, fp)
			return
		case st.IsDir():
			if !strings.HasSuffix(r.URL.Path, "/") {
				target := r.URL.Path + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			}
			if isFile(filepath.Join(fp, "index.php")) {
				s.php(w, r, path.Join(upath, "index.php"), upath, "")
				return
			}
			if isFile(filepath.Join(fp, "index.html")) {
				s.static(w, r, filepath.Join(fp, "index.html"))
				return
			}
		}
	}

	// Front controller: pretty URLs.
	if isFile(filepath.Join(s.docroot, "index.php")) {
		s.php(w, r, "/index.php", upath, "")
		return
	}
	s.cfg.Error(w, r, http.StatusNotFound)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func (s *Site) static(w http.ResponseWriter, r *http.Request, fp string) {
	real, err := filepath.EvalSymlinks(fp)
	inData := s.dataReal != "" && err == nil && within(real, s.dataReal)
	if err != nil || !(within(real, s.buildReal) || inData) || phpLike(real) || phpLike(fp) {
		s.cfg.Error(w, r, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.cfg.Error(w, r, http.StatusMethodNotAllowed)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		s.cfg.Error(w, r, http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		s.cfg.Error(w, r, http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType(real))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", fmt.Sprintf(`W/"%x-%x"`, st.Size(), st.ModTime().UnixNano()))
	if inData {
		// Uploaded files: an .html or .svg upload must not run scripts on
		// the app's own origin.
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; media-src 'self'")
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// hop-by-hop and CGI-only response headers that never go to the visitor.
var dropResponse = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Transfer-Encoding": true, "Te": true,
	"Trailer": true, "Upgrade": true, "Proxy-Connection": true, "Status": true,
}

func (s *Site) php(w http.ResponseWriter, r *http.Request, script, uri, pathInfo string) {
	fp := filepath.Join(s.docroot, script)
	real, err := filepath.EvalSymlinks(fp)
	if err != nil || !within(real, s.buildReal) || (s.dataReal != "" && within(real, s.dataReal)) {
		s.cfg.Error(w, r, http.StatusNotFound) // PHP outside the build never runs (P14)
		return
	}

	body, length, cleanup, code := s.requestBody(r)
	if code != 0 {
		s.cfg.Error(w, r, code)
		return
	}
	defer cleanup()

	params := s.params(r, script, uri, pathInfo, fp, length)
	start := time.Now()
	resp, err := s.cfg.FastCGI.Do(r.Context(), &fastcgi.Request{
		Params: params,
		Body:   body,
		Stderr: func(b []byte) { s.logPHP(r, b) },
	})
	if err != nil {
		if r.Context().Err() != nil {
			return // the visitor went away
		}
		code := http.StatusBadGateway
		if errors.Is(err, fastcgi.ErrUnavailable) {
			code = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "3")
		}
		if s.cfg.PHPLog != nil {
			s.cfg.PHPLog(r, fmt.Sprintf("phpboat: FastCGI error after %s: %v", time.Since(start).Round(time.Millisecond), err))
		}
		s.cfg.Error(w, r, code)
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	for k, vs := range resp.Header {
		if dropResponse[k] {
			continue
		}
		h[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	copyFlushing(w, resp.Body)
}

// copyFlushing streams the body and flushes whenever nothing more is
// buffered from FPM, so flush()/SSE output reaches the visitor at once
// without a flush per chunk on fast responses.
func copyFlushing(w http.ResponseWriter, b *fastcgi.Body) {
	rc := http.NewResponseController(w)
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)
	for {
		n, err := b.Read(*buf)
		if n > 0 {
			if _, werr := w.Write((*buf)[:n]); werr != nil {
				return
			}
			if b.Buffered() == 0 {
				if rc.Flush() != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Site) logPHP(r *http.Request, b []byte) {
	if s.cfg.PHPLog == nil {
		return
	}
	// FPM joins several messages of one request into one record:
	// "PHP message: A; PHP message: B".
	text := strings.ReplaceAll(string(b), "; PHP message: ", "\nPHP message: ")
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s.cfg.PHPLog(r, line)
		}
	}
}

// requestBody returns the body to send and its length. Bodies of unknown
// length (chunked) are spooled to a temporary file and counted, because
// FastCGI needs CONTENT_LENGTH.
func (s *Site) requestBody(r *http.Request) (io.Reader, int64, func(), int) {
	nop := func() {}
	if r.ContentLength > MaxBodyBytes {
		return nil, 0, nop, http.StatusRequestEntityTooLarge
	}
	if r.ContentLength >= 0 {
		if r.ContentLength == 0 {
			return nil, 0, nop, 0
		}
		return io.LimitReader(r.Body, r.ContentLength), r.ContentLength, nop, 0
	}
	f, err := os.CreateTemp(s.cfg.SpoolDir, "phpboat-body-*")
	if err != nil {
		return nil, 0, nop, http.StatusInternalServerError
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	n, err := io.Copy(f, io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		cleanup()
		return nil, 0, nop, http.StatusBadRequest
	}
	if n > MaxBodyBytes {
		cleanup()
		return nil, 0, nop, http.StatusRequestEntityTooLarge
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, nop, http.StatusInternalServerError
	}
	return f, n, cleanup, 0
}

// skipRequest: headers that never become HTTP_* params. Proxy: httpoxy
// (HTTP_PROXY). The forwarding headers: phpboat sets the true values as
// REMOTE_ADDR/HTTPS itself, so nothing a visitor sends can claim them.
var skipRequest = map[string]bool{
	"Proxy": true, "X-Forwarded-For": true, "X-Forwarded-Proto": true, "X-Forwarded-Host": true,
	"X-Forwarded-Port": true, "X-Forwarded-Ssl": true, "X-Real-Ip": true, "Forwarded": true,
	"Content-Type": true, "Content-Length": true, "Connection": true, "Te": true, "Upgrade": true,
}

func (s *Site) params(r *http.Request, script, uri, pathInfo, filename string, length int64) map[string]string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	_, rport, _ := net.SplitHostPort(r.RemoteAddr)
	p := make(map[string]string, 24+len(r.Header))
	p["GATEWAY_INTERFACE"] = "CGI/1.1"
	p["SERVER_SOFTWARE"] = "phpboat"
	p["SERVER_PROTOCOL"] = r.Proto
	p["SERVER_NAME"] = host
	p["SERVER_ADDR"] = "127.0.0.1"
	p["SERVER_PORT"] = "443"
	p["HTTPS"] = "on"
	p["REQUEST_SCHEME"] = "https"
	p["REDIRECT_STATUS"] = "200"
	p["REQUEST_METHOD"] = r.Method
	p["REQUEST_URI"] = r.RequestURI
	p["QUERY_STRING"] = r.URL.RawQuery
	p["DOCUMENT_ROOT"] = s.docroot
	p["DOCUMENT_URI"] = uri
	p["SCRIPT_NAME"] = script
	p["SCRIPT_FILENAME"] = filename
	p["REMOTE_ADDR"] = s.cfg.ClientIP(r)
	p["REMOTE_PORT"] = rport
	p["CONTENT_LENGTH"] = ""
	if length > 0 || r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		p["CONTENT_LENGTH"] = strconv.FormatInt(length, 10)
	}
	p["CONTENT_TYPE"] = r.Header.Get("Content-Type")
	if pathInfo != "" {
		p["PATH_INFO"] = pathInfo
		p["PATH_TRANSLATED"] = s.docroot + pathInfo
	}
	for k, vs := range r.Header {
		// Underscores: "X_Forwarded_For" would become HTTP_X_FORWARDED_FOR
		// (nginx drops these by default too).
		if skipRequest[k] || strings.ContainsRune(k, '_') {
			continue
		}
		p["HTTP_"+cgiName(k)] = strings.Join(vs, ", ")
	}
	p["HTTP_HOST"] = host
	return p
}

func cgiName(k string) string {
	b := make([]byte, len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		case c == '-':
			c = '_'
		}
		b[i] = c
	}
	return string(b)
}

// Ping runs a GET for path through the full handler and returns the
// status (health checks, plan §8).
func (s *Site) Ping(ctx context.Context, host, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return 0, err
	}
	req.RequestURI = path
	req.RemoteAddr = "127.0.0.1:0"
	rec := &discardWriter{h: http.Header{}}
	s.ServeHTTP(rec, req)
	if rec.code == 0 {
		return 0, errors.New("no response")
	}
	return rec.code, nil
}

type discardWriter struct {
	h    http.Header
	code int
}

func (d *discardWriter) Header() http.Header { return d.h }
func (d *discardWriter) Write(b []byte) (int, error) {
	if d.code == 0 {
		d.code = http.StatusOK
	}
	return len(b), nil
}
func (d *discardWriter) Flush() {}
func (d *discardWriter) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}
