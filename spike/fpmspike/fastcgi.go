package main

// A deliberately small FastCGI client (responder role, one request per
// connection). Throwaway: phase 2 writes the real one in internal/fastcgi.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fcgiBeginRequest = 1
	fcgiEndRequest   = 3
	fcgiParams       = 4
	fcgiStdin        = 5
	fcgiStdout       = 6
	fcgiStderr       = 7
	fcgiResponder    = 1
	fcgiMaxContent   = 65535
	fcgiRequestID    = 1
)

type fcgiResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser // the rest of FCGI_STDOUT
}

func writeRecord(w io.Writer, typ uint8, content []byte) error {
	pad := (8 - len(content)%8) % 8
	h := [8]byte{1, typ, 0, fcgiRequestID, byte(len(content) >> 8), byte(len(content)), byte(pad), 0}
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if _, err := w.Write(content); err != nil {
		return err
	}
	_, err := w.Write(make([]byte, pad))
	return err
}

func appendLen(b []byte, n int) []byte {
	if n < 128 {
		return append(b, byte(n))
	}
	return binary.BigEndian.AppendUint32(b, uint32(n)|1<<31)
}

func encodeParams(params map[string]string) []byte {
	var b []byte
	for k, v := range params {
		b = appendLen(b, len(k))
		b = appendLen(b, len(v))
		b = append(b, k...)
		b = append(b, v...)
	}
	return b
}

// writeStream sends r as records of typ, then the empty record that ends it.
func writeStream(w io.Writer, typ uint8, data []byte, r io.Reader) error {
	if r == nil {
		r = strings.NewReader("")
	}
	for len(data) > 0 {
		n := min(len(data), fcgiMaxContent)
		if err := writeRecord(w, typ, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	bp := chunkPool.Get().(*[]byte)
	defer chunkPool.Put(bp)
	buf := *bp
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := writeRecord(w, typ, buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return writeRecord(w, typ, nil)
}

var (
	writerPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 16<<10) }}
	readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 16<<10) }}
	chunkPool  = sync.Pool{New: func() any { b := make([]byte, 16<<10); return &b }}
)

// recordReader returns FCGI_STDOUT content on demand, straight from the
// connection (no goroutine, no pipe); FCGI_STDERR goes to stderr.
type recordReader struct {
	conn   net.Conn
	br     *bufio.Reader
	left   int // stdout bytes left in the current record
	pad    int
	stderr func(string)
	done   bool
	err    error
}

func (rr *recordReader) Read(p []byte) (int, error) {
	for rr.left == 0 {
		if rr.done {
			return 0, io.EOF
		}
		if rr.err != nil {
			return 0, rr.err
		}
		if rr.pad > 0 {
			if _, err := rr.br.Discard(rr.pad); err != nil {
				rr.err = err
				return 0, err
			}
			rr.pad = 0
		}
		h, err := rr.br.Peek(8)
		if err != nil {
			rr.err = fmt.Errorf("fastcgi: %w", err)
			return 0, rr.err
		}
		typ, n, pad := h[1], int(binary.BigEndian.Uint16(h[4:6])), int(h[6])
		rr.br.Discard(8)
		switch typ {
		case fcgiStdout:
			rr.left, rr.pad = n, pad
		case fcgiStderr:
			b := make([]byte, n+pad)
			if _, err := io.ReadFull(rr.br, b); err != nil {
				rr.err = err
				return 0, err
			}
			if n > 0 && rr.stderr != nil {
				rr.stderr(string(b[:n]))
			}
		case fcgiEndRequest:
			rr.br.Discard(n + pad)
			rr.done = true
		default:
			rr.br.Discard(n + pad)
		}
	}
	if len(p) > rr.left {
		p = p[:rr.left]
	}
	n, err := rr.br.Read(p)
	rr.left -= n
	if err != nil {
		rr.err = err
	}
	return n, err
}

func (rr *recordReader) Close() error {
	if rr.br != nil {
		rr.br.Reset(nil)
		readerPool.Put(rr.br)
		rr.br = nil
	}
	return rr.conn.Close()
}

// fcgiDo sends one request to the FPM socket and returns the parsed CGI
// response. stderr receives FCGI_STDERR content (PHP's error log lines).
func fcgiDo(sock string, params map[string]string, body io.Reader, stderr func(string)) (*fcgiResponse, error) {
	conn, err := net.DialTimeout("unix", sock, 10*time.Second)
	if err != nil {
		return nil, err
	}
	bw := writerPool.Get().(*bufio.Writer)
	bw.Reset(conn)
	begin := []byte{0, fcgiResponder, 0, 0, 0, 0, 0, 0} // role, flags=0 (close after request)
	err = writeRecord(bw, fcgiBeginRequest, begin)
	if err == nil {
		err = writeStream(bw, fcgiParams, encodeParams(params), nil)
	}
	if err == nil {
		err = writeStream(bw, fcgiStdin, nil, body)
	}
	if err == nil {
		err = bw.Flush()
	}
	bw.Reset(nil)
	writerPool.Put(bw)
	if err != nil {
		conn.Close()
		return nil, err
	}

	br := readerPool.Get().(*bufio.Reader)
	br.Reset(conn)
	rr := &recordReader{conn: conn, br: br, stderr: stderr}
	hr := bufio.NewReaderSize(rr, 4096)
	hdr, err := textproto.NewReader(hr).ReadMIMEHeader()
	if err != nil {
		rr.Close()
		return nil, fmt.Errorf("fastcgi: reading response headers: %w", err)
	}
	resp := &fcgiResponse{Status: http.StatusOK, Header: http.Header(hdr), Body: struct {
		io.Reader
		io.Closer
	}{hr, rr}}
	if s := hdr.Get("Status"); s != "" {
		code, err := strconv.Atoi(strings.Fields(s)[0])
		if err != nil || code < 100 || code > 999 {
			rr.Close()
			return nil, fmt.Errorf("fastcgi: bad Status %q", s)
		}
		resp.Status = code
		resp.Header.Del("Status")
	} else if hdr.Get("Location") != "" {
		resp.Status = http.StatusFound
	}
	return resp, nil
}
