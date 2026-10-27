package phpserve

import (
	"path/filepath"
	"strings"
	"sync"
)

// A fixed table, so the Content-Type never depends on the server's
// /etc/mime.types. Anything else is application/octet-stream (never
// sniffed: nosniff is always set).
var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".json": "application/json", ".map": "application/json", ".webmanifest": "application/manifest+json",
	".xml": "application/xml", ".rss": "application/rss+xml", ".atom": "application/atom+xml",
	".txt": "text/plain; charset=utf-8", ".md": "text/plain; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".ics": "text/calendar; charset=utf-8", ".vtt": "text/vtt; charset=utf-8",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif", ".ico": "image/x-icon", ".bmp": "image/bmp",
	".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
	".mp4": "video/mp4", ".webm": "video/webm", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".wav": "audio/wav", ".m4a": "audio/mp4", ".flac": "audio/flac",
	".pdf": "application/pdf", ".zip": "application/zip", ".gz": "application/gzip", ".wasm": "application/wasm",
}

func contentType(name string) string {
	if t, ok := mimeTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}
