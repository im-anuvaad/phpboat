package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// site serves one app: static files itself, PHP through FastCGI
// (docs/plan.md §4).
type site struct {
	app       *App
	buildReal string // resolved build dir
	dataReal  string // resolved DATA_DIR
	docroot   string // build/<docroot>, unresolved (the path PHP sees)
	rootMode  bool   // docroot = "." → extra denylist
}

var rootDenyFiles = map[string]bool{"composer.json": true, "composer.lock": true, "phpboat.toml": true}
var rootDenyExt = map[string]bool{".sqlite": true, ".sqlite3": true, ".db": true, ".sql": true, ".log": true}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := path.Clean("/" + r.URL.Path)
	segs := strings.Split(strings.TrimPrefix(upath, "/"), "/")
	for _, seg := range segs {
		if strings.HasPrefix(seg, ".") {
			http.NotFound(w, r)
			return
		}
	}
	if s.rootMode && len(segs) > 0 {
		last := segs[len(segs)-1]
		if segs[0] == "vendor" || rootDenyFiles[last] || rootDenyExt[strings.ToLower(filepath.Ext(last))] {
			http.NotFound(w, r)
			return
		}
	}

	// 1. /a/b.php/extra → script /a/b.php, PATH_INFO /extra
	prefix := ""
	for i, seg := range segs {
		prefix += "/" + seg
		if strings.HasSuffix(seg, ".php") {
			if st, err := os.Stat(filepath.Join(s.docroot, prefix)); err == nil && st.Mode().IsRegular() {
				s.php(w, r, prefix, strings.Join(append([]string{""}, segs[i+1:]...), "/"))
				return
			}
		}
	}

	fp := filepath.Join(s.docroot, upath)
	if st, err := os.Stat(fp); err == nil {
		if st.IsDir() {
			if !strings.HasSuffix(r.URL.Path, "/") {
				http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
				return
			}
			if fileExists(filepath.Join(fp, "index.php")) {
				s.php(w, r, path.Join(upath, "index.php"), "")
				return
			}
			if fileExists(filepath.Join(fp, "index.html")) {
				s.static(w, r, filepath.Join(fp, "index.html"))
				return
			}
		} else if st.Mode().IsRegular() {
			s.static(w, r, fp)
			return
		}
	}

	// Front controller fallback.
	if fileExists(filepath.Join(s.docroot, "index.php")) {
		s.php(w, r, "/index.php", "")
		return
	}
	http.NotFound(w, r)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

func (s *site) static(w http.ResponseWriter, r *http.Request, fp string) {
	real, err := filepath.EvalSymlinks(fp)
	if err != nil || !(within(real, s.buildReal) || within(real, s.dataReal)) {
		http.NotFound(w, r)
		return
	}
	if strings.EqualFold(filepath.Ext(real), ".php") { // a .php in a persist folder: never run, never shown
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(real)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	ct := mime.TypeByExtension(filepath.Ext(real))
	if ct == "" {
		ct = "application/octet-stream" // never sniff
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *site) php(w http.ResponseWriter, r *http.Request, script, pathInfo string) {
	fp := filepath.Join(s.docroot, script)
	real, err := filepath.EvalSymlinks(fp)
	if err != nil || !within(real, s.buildReal) {
		http.NotFound(w, r) // PHP outside the build never runs (P14)
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	remote, rport, _ := net.SplitHostPort(r.RemoteAddr)
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
		remote = cf
	}
	p := map[string]string{
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_SOFTWARE":   "phpboat-spike",
		"SERVER_PROTOCOL":   r.Proto,
		"REQUEST_METHOD":    r.Method,
		"REQUEST_URI":       r.RequestURI,
		"QUERY_STRING":      r.URL.RawQuery,
		"DOCUMENT_ROOT":     s.docroot,
		"DOCUMENT_URI":      script,
		"SCRIPT_NAME":       script,
		"SCRIPT_FILENAME":   fp,
		"REQUEST_SCHEME":    "https",
		"HTTPS":             "on",
		"SERVER_NAME":       host,
		"SERVER_PORT":       "443",
		"SERVER_ADDR":       "127.0.0.1",
		"REMOTE_ADDR":       remote,
		"REMOTE_PORT":       rport,
		"HTTP_HOST":         host,
	}
	if pathInfo != "" {
		p["PATH_INFO"] = pathInfo
		p["PATH_TRANSLATED"] = s.docroot + pathInfo
	}
	for k, vs := range r.Header {
		switch k {
		case "Proxy", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip", "Content-Type", "Content-Length":
			continue // Proxy: httpoxy; X-Forwarded-*: phpboat sets the truth itself
		}
		p["HTTP_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))] = strings.Join(vs, ", ")
	}
	var body io.Reader = r.Body
	if r.ContentLength >= 0 {
		p["CONTENT_LENGTH"] = strconv.FormatInt(r.ContentLength, 10)
	} else {
		b, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		p["CONTENT_LENGTH"] = strconv.Itoa(len(b))
		body = bytes.NewReader(b)
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		p["CONTENT_TYPE"] = ct
	}

	start := time.Now()
	resp, err := fcgiDo(s.app.Sock(), p, body, func(line string) {
		log.Printf("[%s] php: %s %s: %s", s.app.Name, r.Method, r.URL.Path, strings.TrimSpace(line))
	})
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrDeadlineExceeded) {
			code = http.StatusServiceUnavailable
		}
		log.Printf("[%s] fastcgi error after %s: %v", s.app.Name, time.Since(start), err)
		http.Error(w, http.StatusText(code), code)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.Status)
	fl, _ := w.(http.Flusher)
	bp := chunkPool.Get().(*[]byte)
	defer chunkPool.Put(bp)
	buf := *bp
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
