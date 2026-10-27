// Package fcgitest is a fake PHP-FPM for tests: a FastCGI responder on a
// unix socket that hands each request to a Go function.
package fcgitest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// Request is what the fake received.
type Request struct {
	Params map[string]string
	Stdin  []byte
}

// Response is what it answers. Raw, if set, is sent as FCGI_STDOUT
// verbatim (headers + body); otherwise Status/Header/Body are rendered.
type Response struct {
	Status int
	Header map[string]string
	Body   string
	Stderr string
	Raw    string
}

// Server is a running fake.
type Server struct {
	Socket string
	ln     net.Listener
	mu     sync.Mutex
	last   *Request
}

// Last returns the most recent request.
func (s *Server) Last() *Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// New starts a fake answering with handle; it stops with the test.
func New(t testing.TB, handle func(*Request) Response) *Server {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "php.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Socket: sock, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, handle)
		}
	}()
	return s
}

func (s *Server) serve(c net.Conn, handle func(*Request) Response) {
	defer c.Close()
	req, err := read(c)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.last = req
	s.mu.Unlock()
	resp := handle(req)
	out := resp.Raw
	if out == "" {
		if resp.Status != 0 {
			out += "Status: " + itoa(resp.Status) + "\r\n"
		}
		for k, v := range resp.Header {
			out += k + ": " + v + "\r\n"
		}
		out += "\r\n" + resp.Body
	}
	w := bufio.NewWriter(c)
	if resp.Stderr != "" {
		write(w, 7, resp.Stderr)
	}
	write(w, 6, out)
	write(w, 6, "")
	record(w, 3, []byte{0, 0, 0, 0, 0, 0, 0, 0})
	w.Flush()
}

func itoa(n int) string {
	var b [8]byte
	i := len(b)
	for n > 0 || i == len(b) {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func write(w *bufio.Writer, typ byte, s string) {
	for len(s) > 0 {
		n := min(len(s), 65535)
		record(w, typ, []byte(s[:n]))
		s = s[n:]
	}
}

func record(w *bufio.Writer, typ byte, b []byte) {
	pad := (8 - len(b)%8) % 8
	w.Write([]byte{1, typ, 0, 1, byte(len(b) >> 8), byte(len(b)), byte(pad), 0})
	w.Write(b)
	w.Write(make([]byte, pad))
}

func read(c net.Conn) (*Request, error) {
	br := bufio.NewReader(c)
	req := &Request{}
	var params []byte
	for {
		var h [8]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return nil, err
		}
		n, pad := int(binary.BigEndian.Uint16(h[4:6])), int(h[6])
		b := make([]byte, n+pad)
		if _, err := io.ReadFull(br, b); err != nil {
			return nil, err
		}
		b = b[:n]
		switch h[1] {
		case 4:
			params = append(params, b...)
		case 5:
			if n == 0 {
				p, err := decode(params)
				req.Params = p
				return req, err
			}
			req.Stdin = append(req.Stdin, b...)
		}
	}
}

func decode(b []byte) (map[string]string, error) {
	out := map[string]string{}
	l := func() (int, error) {
		if len(b) == 0 {
			return 0, errors.New("short params")
		}
		if b[0]>>7 == 0 {
			n := int(b[0])
			b = b[1:]
			return n, nil
		}
		if len(b) < 4 {
			return 0, errors.New("short params")
		}
		n := int(binary.BigEndian.Uint32(b) &^ (1 << 31))
		b = b[4:]
		return n, nil
	}
	for len(b) > 0 {
		k, err := l()
		if err != nil {
			return nil, err
		}
		v, err := l()
		if err != nil {
			return nil, err
		}
		if k+v > len(b) {
			return nil, errors.New("short params")
		}
		out[string(b[:k])] = string(b[k : k+v])
		b = b[k+v:]
	}
	return out, nil
}
