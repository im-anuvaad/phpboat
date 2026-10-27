package fastcgi

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Record types and roles (FastCGI 1.0 spec).
const (
	typeBeginRequest = 1
	typeAbortRequest = 2
	typeEndRequest   = 3
	typeParams       = 4
	typeStdin        = 5
	typeStdout       = 6
	typeStderr       = 7

	roleResponder = 1

	// protocolStatus in FCGI_END_REQUEST.
	statusRequestComplete = 0
	statusCantMpxConn     = 1
	statusOverloaded      = 2
	statusUnknownRole     = 3

	maxContent = 65535
	requestID  = 1
	headerLen  = 8
)

var errHeaderTooLarge = errors.New("fastcgi: header block too large")

var zeroPad [8]byte

func writeRecord(w *bufio.Writer, typ byte, content []byte) error {
	if len(content) > maxContent {
		return fmt.Errorf("fastcgi: record of %d bytes", len(content))
	}
	pad := (8 - len(content)%8) % 8
	h := [headerLen]byte{1, typ, 0, requestID, byte(len(content) >> 8), byte(len(content)), byte(pad), 0}
	w.Write(h[:])
	w.Write(content)
	_, err := w.Write(zeroPad[:pad])
	return err
}

// writeStream writes data as records of typ, then the empty record that
// ends the stream.
func writeStream(w *bufio.Writer, typ byte, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), maxContent)
		if err := writeRecord(w, typ, data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return writeRecord(w, typ, nil)
}

func appendLen(b []byte, n int) []byte {
	if n < 128 {
		return append(b, byte(n))
	}
	return binary.BigEndian.AppendUint32(b, uint32(n)|1<<31)
}

// encodeParams appends the name-value pairs to b. Names and values longer
// than 2^31-1 bytes can't happen here (headers are bounded upstream).
func encodeParams(b []byte, params map[string]string) []byte {
	for k, v := range params {
		b = appendLen(b, len(k))
		b = appendLen(b, len(v))
		b = append(b, k...)
		b = append(b, v...)
	}
	return b
}

// decodeParams parses name-value pairs (tests and the fake server).
func decodeParams(b []byte) (map[string]string, error) {
	out := map[string]string{}
	readLen := func() (int, error) {
		if len(b) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		if b[0]>>7 == 0 {
			n := int(b[0])
			b = b[1:]
			return n, nil
		}
		if len(b) < 4 {
			return 0, io.ErrUnexpectedEOF
		}
		n := int(binary.BigEndian.Uint32(b) &^ (1 << 31))
		b = b[4:]
		return n, nil
	}
	for len(b) > 0 {
		kl, err := readLen()
		if err != nil {
			return nil, err
		}
		vl, err := readLen()
		if err != nil {
			return nil, err
		}
		if kl+vl > len(b) || kl < 0 || vl < 0 {
			return nil, io.ErrUnexpectedEOF
		}
		out[string(b[:kl])] = string(b[kl : kl+vl])
		b = b[kl+vl:]
	}
	return out, nil
}

// recordReader yields FCGI_STDOUT content straight from the connection,
// hands FCGI_STDERR to stderr and stops at FCGI_END_REQUEST.
type recordReader struct {
	br     *bufio.Reader
	stderr func([]byte)
	left   int // stdout bytes left in the current record
	pad    int // padding after it
	done   bool
	err    error
	// limit, when >= 0, is how many more stdout bytes may be read (the
	// header block); exceeding it fails with errHeaderTooLarge.
	limit int
	init  bool
}

func (rr *recordReader) Read(p []byte) (int, error) {
	if !rr.init {
		rr.init = true
		rr.limit = MaxHeaderBytes + 4096 // + one read-ahead buffer of the header parser
	}
	for rr.left == 0 {
		if rr.done {
			return 0, io.EOF
		}
		if rr.err != nil {
			return 0, rr.err
		}
		if err := rr.next(); err != nil {
			rr.err = err
			return 0, err
		}
	}
	if len(p) > rr.left {
		p = p[:rr.left]
	}
	if rr.limit >= 0 {
		if rr.limit == 0 {
			rr.err = errHeaderTooLarge
			return 0, rr.err
		}
		if len(p) > rr.limit {
			p = p[:rr.limit]
		}
	}
	n, err := rr.br.Read(p)
	rr.left -= n
	if rr.limit >= 0 {
		rr.limit -= n
	}
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF // EOF inside a record
		}
		rr.err = fmt.Errorf("fastcgi: %w", err)
		return n, rr.err
	}
	return n, nil
}

// next consumes records until stdout content is available or the request
// ended.
func (rr *recordReader) next() error {
	if rr.pad > 0 {
		if _, err := rr.br.Discard(rr.pad); err != nil {
			return fmt.Errorf("fastcgi: %w", unexpected(err))
		}
		rr.pad = 0
	}
	h, err := rr.br.Peek(headerLen)
	if err != nil {
		if err == io.EOF && len(h) == 0 {
			return fmt.Errorf("fastcgi: connection closed before the end of the request: %w", io.ErrUnexpectedEOF)
		}
		return fmt.Errorf("fastcgi: %w", unexpected(err))
	}
	if h[0] != 1 {
		return fmt.Errorf("fastcgi: bad record version %d", h[0])
	}
	typ, n, pad := h[1], int(binary.BigEndian.Uint16(h[4:6])), int(h[6])
	rr.br.Discard(headerLen)
	switch typ {
	case typeStdout:
		rr.left, rr.pad = n, pad
		return nil
	case typeStderr:
		if n > 0 {
			if rr.stderr == nil {
				if _, err := rr.br.Discard(n); err != nil {
					return fmt.Errorf("fastcgi: %w", unexpected(err))
				}
			} else {
				b := make([]byte, n) // rare (PHP warnings), may exceed the read buffer
				if _, err := io.ReadFull(rr.br, b); err != nil {
					return fmt.Errorf("fastcgi: %w", unexpected(err))
				}
				rr.stderr(b)
			}
		}
		_, err := rr.br.Discard(pad)
		return unexpected(err)
	case typeEndRequest:
		b, err := rr.br.Peek(n)
		if err != nil {
			return fmt.Errorf("fastcgi: %w", unexpected(err))
		}
		status := byte(statusRequestComplete)
		if n >= 5 {
			status = b[4]
		}
		rr.br.Discard(n + pad)
		rr.done = true
		switch status {
		case statusRequestComplete:
			return nil
		case statusOverloaded:
			return ErrOverloaded
		default:
			return fmt.Errorf("fastcgi: request rejected (protocol status %d)", status)
		}
	default: // unknown or management records: skip
		_, err := rr.br.Discard(n + pad)
		return unexpected(err)
	}
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
