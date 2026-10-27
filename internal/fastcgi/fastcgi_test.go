package fastcgi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFPM accepts one connection per request, parses it, and lets the test
// answer with raw records.
type fakeFPM struct {
	t    *testing.T
	ln   net.Listener
	reqs chan *gotRequest
}

type gotRequest struct {
	params map[string]string
	stdin  []byte
	conn   net.Conn
}

func newFake(t *testing.T, answer func(g *gotRequest, w *bufio.Writer)) (*fakeFPM, *Client) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "php.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFPM{t: t, ln: ln, reqs: make(chan *gotRequest, 16)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				g, err := readRequest(c)
				if err != nil {
					return
				}
				g.conn = c
				f.reqs <- g
				w := bufio.NewWriter(c)
				answer(g, w)
				w.Flush()
			}()
		}
	}()
	return f, &Client{Address: sock}
}

func readRequest(c net.Conn) (*gotRequest, error) {
	br := bufio.NewReader(c)
	g := &gotRequest{}
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
		case typeParams:
			params = append(params, b...)
		case typeStdin:
			if n == 0 {
				p, err := decodeParams(params)
				if err != nil {
					return nil, err
				}
				g.params = p
				return g, nil
			}
			g.stdin = append(g.stdin, b...)
		}
	}
}

func rec(w *bufio.Writer, typ byte, content string) {
	for first := true; first || len(content) > 0; first = false {
		n := min(len(content), maxContent)
		writeRecord(w, typ, []byte(content[:n])) // the client may hang up early (that's what some tests check)
		content = content[n:]
	}
}

func end(w *bufio.Writer, status byte) {
	writeRecord(w, typeEndRequest, []byte{0, 0, 0, 0, status, 0, 0, 0})
}

func TestParamsRoundTrip(t *testing.T) {
	in := map[string]string{"A": "1", "LONG_VALUE": strings.Repeat("v", 300), strings.Repeat("K", 200): "", "EMPTY": ""}
	out, err := decodeParams(encodeParams(nil, in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d params, want %d", len(out), len(in))
	}
	for k, v := range in {
		if out[k] != v {
			t.Errorf("%q: got %d bytes, want %d", k[:min(len(k), 10)], len(out[k]), len(v))
		}
	}
}

func TestRequestAndResponse(t *testing.T) {
	var stderrMu sync.Mutex
	var stderr []string
	_, c := newFake(t, func(g *gotRequest, w *bufio.Writer) {
		rec(w, typeStdout, "Status: 201 Created\r\nX-A: 1\r\nSet-Cookie: a=1\r\n")
		rec(w, typeStderr, "PHP Notice: something")
		rec(w, typeStdout, "Set-Cookie: b=2\r\nContent-Type: text/plain\r\n\r\nhel")
		rec(w, typeStdout, "lo ")
		rec(w, typeStdout, "") // empty stdout records are legal mid-stream? FPM never sends one before the end, but tolerate it
		rec(w, typeStdout, string(g.stdin)+" "+g.params["BIG"][:3])
		end(w, statusRequestComplete)
	})
	big := strings.Repeat("x", 70000) // params larger than one record
	body := strings.Repeat("b", 100000)
	resp, err := c.Do(context.Background(), &Request{
		Params: map[string]string{"BIG": big, "CONTENT_LENGTH": "100000"},
		Body:   strings.NewReader(body),
		Stderr: func(b []byte) { stderrMu.Lock(); stderr = append(stderr, string(b)); stderrMu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("Status") != "" || resp.Header.Get("X-A") != "1" {
		t.Errorf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 {
		t.Errorf("Set-Cookie %v", got)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := "hello " + body + " xxx"; string(b) != want {
		t.Errorf("body %d bytes, want %d", len(b), len(want))
	}
	stderrMu.Lock()
	defer stderrMu.Unlock()
	if len(stderr) != 1 || stderr[0] != "PHP Notice: something" {
		t.Errorf("stderr %q", stderr)
	}
}

func TestLocationImpliesFound(t *testing.T) {
	_, c := newFake(t, func(g *gotRequest, w *bufio.Writer) {
		rec(w, typeStdout, "Location: /x\r\n\r\n")
		end(w, statusRequestComplete)
	})
	resp, err := c.Do(context.Background(), &Request{Params: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name   string
		answer func(*gotRequest, *bufio.Writer)
		want   string
		is     error
	}{
		{"overloaded", func(_ *gotRequest, w *bufio.Writer) { end(w, statusOverloaded) }, "overloaded", ErrUnavailable},
		{"unknown role", func(_ *gotRequest, w *bufio.Writer) { end(w, statusUnknownRole) }, "protocol status 3", nil},
		{"closed early", func(_ *gotRequest, w *bufio.Writer) {}, "connection closed", nil},
		{"closed inside a record", func(_ *gotRequest, w *bufio.Writer) { w.Write([]byte{1, typeStdout, 0, 1, 0, 100, 0, 0, 'S'}) }, "unexpected EOF", nil},
		{"bad version", func(_ *gotRequest, w *bufio.Writer) { w.Write([]byte{2, typeStdout, 0, 1, 0, 0, 0, 0}) }, "version", nil},
		{"bad status", func(_ *gotRequest, w *bufio.Writer) { rec(w, typeStdout, "Status: abc\r\n\r\n"); end(w, 0) }, "bad Status", nil},
		{"headers too large", func(_ *gotRequest, w *bufio.Writer) {
			for i := 0; i < 20; i++ {
				rec(w, typeStdout, "X-Big: "+strings.Repeat("a", 8000)+"\r\n")
			}
			end(w, 0)
		}, "larger than", nil},
		{"no header terminator", func(_ *gotRequest, w *bufio.Writer) { rec(w, typeStdout, "X-A: 1\r\n"); end(w, 0) }, "no valid response headers", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newFake(t, tc.answer)
			resp, err := c.Do(context.Background(), &Request{Params: map[string]string{}})
			if err == nil {
				resp.Body.Close()
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error %v is not %v", err, tc.is)
			}
		})
	}
}

func TestUnavailable(t *testing.T) {
	c := &Client{Address: filepath.Join(t.TempDir(), "missing.sock")}
	_, err := c.Do(context.Background(), &Request{})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("missing socket: %v", err)
	}
	// A socket file nobody listens on.
	sock := filepath.Join(t.TempDir(), "dead.sock")
	ln, _ := net.Listen("unix", sock)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	_, err = (&Client{Address: sock}).Do(context.Background(), &Request{})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("dead socket: %v", err)
	}
}

func TestCancelWhileStreaming(t *testing.T) {
	release := make(chan struct{})
	_, c := newFake(t, func(g *gotRequest, w *bufio.Writer) {
		rec(w, typeStdout, "Content-Type: text/plain\r\n\r\nfirst")
		w.Flush()
		<-release // PHP is "busy"
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := c.Do(ctx, &Request{Params: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "first" {
		t.Fatalf("%q %v", buf, err)
	}
	if resp.Body.Buffered() != 0 {
		t.Errorf("buffered %d after reading everything sent", resp.Body.Buffered())
	}
	done := make(chan error, 1)
	go func() { _, err := resp.Body.Read(buf); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("read after cancel succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read not interrupted by cancel")
	}
}

func TestBodyCloseBeforeEnd(t *testing.T) {
	closed := make(chan struct{})
	_, c := newFake(t, func(g *gotRequest, w *bufio.Writer) {
		rec(w, typeStdout, "\r\n"+strings.Repeat("z", 1000))
		w.Flush()
		g.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		g.conn.Read(make([]byte, 1)) // returns when the client closes
		close(closed)
	})
	resp, err := c.Do(context.Background(), &Request{Params: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp.Body.Close() // twice is fine
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("connection not closed")
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Error("read on closed body succeeded")
	}
}

// FuzzResponse feeds arbitrary bytes as FPM's answer: no panics, no hangs.
func FuzzResponse(f *testing.F) {
	var seed bytes.Buffer
	w := bufio.NewWriter(&seed)
	rec(w, typeStdout, "Status: 200 OK\r\nA: b\r\n\r\nbody")
	rec(w, typeStderr, "warn")
	end(w, 0)
	w.Flush()
	f.Add(seed.Bytes())
	f.Add([]byte{1, 6, 0, 1, 255, 255, 7, 0})
	f.Add([]byte{1, 3, 0, 1, 0, 8, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		go func() {
			readRequest(b)
			b.Write(data)
			b.Close()
		}()
		c := &Client{Dial: func(context.Context) (net.Conn, error) { return a, nil }}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := c.Do(ctx, &Request{Params: map[string]string{"A": "b"}, Stderr: func([]byte) {}})
		if err != nil {
			return
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	})
}
