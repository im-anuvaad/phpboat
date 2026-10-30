// Package fastcgi is a small FastCGI client for PHP-FPM (docs/architecture.md §4):
// the responder role, one request per connection, request bodies and
// responses streamed. The Go standard library only has the server side.
//
// It is written for phpboat's hot path (docs/architecture.md §4): buffers are pooled,
// and the response is read straight from the connection by the caller's
// goroutine (no extra goroutine or pipe per request).
package fastcgi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MaxHeaderBytes bounds the CGI response header block.
const MaxHeaderBytes = 64 << 10

// ErrUnavailable wraps errors that mean the pool is not taking requests:
// no socket, nobody listening, or its backlog is full. Callers answer 503;
// any other error is a 502.
var ErrUnavailable = errors.New("fastcgi: php-fpm is not accepting connections")

// ErrOverloaded is returned when FPM ends a request with FCGI_OVERLOADED.
var ErrOverloaded = fmt.Errorf("%w (overloaded)", ErrUnavailable)

// Client sends requests to one FPM pool.
type Client struct {
	Network     string        // "unix" (default) or "tcp"
	Address     string        // socket path or host:port
	DialTimeout time.Duration // default 10 s

	// Dial overrides the dialer (tests).
	Dial func(ctx context.Context) (net.Conn, error)
}

// Request is one FastCGI request.
type Request struct {
	Params map[string]string
	// Body is sent as FCGI_STDIN; nil means empty. Its length must match
	// Params["CONTENT_LENGTH"].
	Body io.Reader
	// Stderr receives FCGI_STDERR content (PHP's log lines), called from
	// the goroutine reading the response. May be nil.
	Stderr func([]byte)
}

// Response is the parsed CGI response. Body must be closed.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       *Body
}

var (
	writerPool = sync.Pool{New: func() any { return bufio.NewWriterSize(nil, 16<<10) }}
	readerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 16<<10) }}
	headerPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 4<<10) }}
	chunkPool  = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}
	paramsPool = sync.Pool{New: func() any { b := make([]byte, 0, 4<<10); return &b }}
)

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx)
	}
	network := c.Network
	if network == "" {
		network = "unix"
	}
	timeout := c.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, network, c.Address)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return nil, err
	}
	return conn, nil
}

// Do sends req and returns the response once its headers have arrived.
// Cancelling ctx aborts the request (the connection is closed), also
// while the body is being read.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })

	werr := writeRequest(conn, req)
	// Even if writing failed (FPM may close early, e.g. on oversized
	// params), a response may already be waiting: try to read it.
	resp, rerr := readResponse(conn, req.Stderr, stop)
	if rerr != nil {
		stop()
		conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if werr != nil {
			return nil, fmt.Errorf("fastcgi: sending request: %w", werr)
		}
		return nil, rerr
	}
	return resp, nil
}

func writeRequest(conn net.Conn, req *Request) error {
	bw := writerPool.Get().(*bufio.Writer)
	bw.Reset(conn)
	defer func() {
		bw.Reset(nil)
		writerPool.Put(bw)
	}()
	if err := writeRecord(bw, typeBeginRequest, []byte{0, roleResponder, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	pb := paramsPool.Get().(*[]byte)
	*pb = encodeParams((*pb)[:0], req.Params)
	err := writeStream(bw, typeParams, *pb)
	if cap(*pb) <= 64<<10 {
		paramsPool.Put(pb)
	}
	if err != nil {
		return err
	}
	if req.Body != nil {
		cp := chunkPool.Get().(*[]byte)
		defer chunkPool.Put(cp)
		buf := *cp
		for {
			n, rerr := req.Body.Read(buf)
			if n > 0 {
				if err := writeRecord(bw, typeStdin, buf[:n]); err != nil {
					return err
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return fmt.Errorf("reading request body: %w", rerr)
			}
		}
	}
	if err := writeRecord(bw, typeStdin, nil); err != nil {
		return err
	}
	return bw.Flush()
}

func readResponse(conn net.Conn, stderr func([]byte), stop func() bool) (*Response, error) {
	br := readerPool.Get().(*bufio.Reader)
	br.Reset(conn)
	b := &Body{conn: conn, rr: recordReader{br: br, stderr: stderr}, stop: stop}
	hr := headerPool.Get().(*bufio.Reader)
	hr.Reset(&b.rr)
	b.hr = hr
	hdr, err := textproto.NewReader(hr).ReadMIMEHeader()
	if err != nil {
		b.release()
		if errors.Is(err, errHeaderTooLarge) {
			return nil, fmt.Errorf("fastcgi: response headers larger than %d bytes", MaxHeaderBytes)
		}
		if b.rr.err != nil && !errors.Is(b.rr.err, io.EOF) {
			return nil, b.rr.err
		}
		return nil, fmt.Errorf("fastcgi: no valid response headers: %w", err)
	}
	b.rr.limit = -1
	resp := &Response{StatusCode: http.StatusOK, Header: http.Header(hdr), Body: b}
	if s := hdr.Get("Status"); s != "" {
		f := strings.Fields(s)
		code, err := strconv.Atoi(f[0])
		if err != nil || code < 100 || code > 999 {
			b.release()
			return nil, fmt.Errorf("fastcgi: bad Status header %q", s)
		}
		resp.StatusCode = code
		resp.Header.Del("Status")
	} else if hdr.Get("Location") != "" {
		resp.StatusCode = http.StatusFound
	}
	return resp, nil
}

// Body is the response body: FCGI_STDOUT after the headers.
type Body struct {
	conn net.Conn
	rr   recordReader
	hr   *bufio.Reader
	stop func() bool
	once sync.Once
}

// Read implements io.Reader.
func (b *Body) Read(p []byte) (int, error) {
	if b.hr == nil {
		return 0, errors.New("fastcgi: read on closed body")
	}
	return b.hr.Read(p)
}

// Buffered reports how many bytes from FPM (body or the records that
// end the request) can be read without waiting. Callers flush their
// output when it is 0, so output is sent when PHP pauses (flush(), slow
// work) and not after every chunk or right before the end anyway.
func (b *Body) Buffered() int {
	if b.hr == nil || b.rr.br == nil {
		return 0
	}
	avail := b.rr.br.Buffered()
	if b.rr.left == 0 {
		// Only the current record's padding, or part of the next header:
		// nothing a reader could get without waiting.
		if avail -= b.rr.pad; avail < headerLen {
			avail = 0
		}
	}
	return b.hr.Buffered() + avail
}

// Close closes the connection (FPM ends the request if PHP is still
// running) and returns the buffers to their pools.
func (b *Body) Close() error {
	b.release()
	return nil
}

func (b *Body) release() {
	b.once.Do(func() {
		b.stop()
		b.conn.Close()
		if b.hr != nil {
			b.hr.Reset(nil)
			headerPool.Put(b.hr)
			b.hr = nil
		}
		if b.rr.br != nil {
			b.rr.br.Reset(nil)
			readerPool.Put(b.rr.br)
			b.rr.br = nil
		}
	})
}
