// Raw HTTP/1.1 server: one goroutine per connection, sequential requests on
// a keep-alive connection, zero framework overhead. This is the hot path for
// every workload, so parsing is allocation-light: headers are scanned in
// place, only the fields the forge reads are kept, and the response is built
// into a per-connection scratch buffer and written with a single Write.
package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

var reqIDCounter uint64

func nextReqID() uint64 { return atomic.AddUint64(&reqIDCounter, 1) }

// request is one parsed HTTP request. Headers the forge reads are kept in
// fields; everything else is dropped.
type request struct {
	method  string
	path    string // decoded path, no query
	rawPath string
	query   string
	version string // "HTTP/1.1" or "HTTP/1.0"
	auth    string
	gitProto string
	host    string
	ctype   string
	contentLength int // -1 when absent
	chunked bool
	expectContinue bool
	close   bool // client asked to close, or HTTP/1.0 without keep-alive
	body    []byte
}

// response is what a handler returns; the server serializes it.
// reqID is the per-request counter rendered as the X-GitHub-Request-Id
// header by serializeHeader, so no request-id string is ever built.
type response struct {
	status  int
	ctype   string // "" means no content-type header
	headers [][2]string // extra headers, in order
	body    []byte
	reqID   int64 // 0 means no X-GitHub-Request-Id header
}

func (r *response) header(k, v string) {
	r.headers = append(r.headers, [2]string{k, v})
}

var statusText = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func writeStatusLine(b []byte, status int) []byte {
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(status), 10)
	b = append(b, ' ')
	if t, ok := statusText[status]; ok {
		b = append(b, t...)
	}
	return append(b, "\r\n"...)
}

// serialize appends the full response (headers + body) to b.
func serialize(b []byte, req *request, resp *response) []byte {
	b = serializeHeader(b, req, resp)
	return append(b, resp.body...)
}

// serializeHeader appends just the status line and headers (with the
// Content-Length) to b.
func serializeHeader(b []byte, req *request, resp *response) []byte {
	b = writeStatusLine(b, resp.status)
	if resp.reqID != 0 {
		b = append(b, "X-GitHub-Request-Id: req-"...)
		b = strconv.AppendInt(b, resp.reqID, 10)
		b = append(b, "\r\n"...)
	}
	if resp.ctype != "" {
		b = append(b, "Content-Type: "...)
		b = append(b, resp.ctype...)
		b = append(b, "\r\n"...)
	}
	for _, h := range resp.headers {
		b = append(b, h[0]...)
		b = append(b, ": "...)
		b = append(b, h[1]...)
		b = append(b, "\r\n"...)
	}
	b = append(b, "Content-Length: "...)
	b = strconv.AppendInt(b, int64(len(resp.body)), 10)
	b = append(b, "\r\n"...)
	if req.close {
		b = append(b, "Connection: close\r\n"...)
	}
	b = append(b, "\r\n"...)
	return b
}

// readLine reads one CRLF-terminated line (without the CRLF) from r.
// It reuses r's buffer when the line fits; longer lines are accumulated.
func readLine(r *bufio.Reader) ([]byte, error) {
	frag, err := r.ReadSlice('\n')
	if err == nil {
		return trimCRLF(frag), nil
	}
	if err != bufio.ErrBufferFull {
		return nil, err
	}
	// Long line: accumulate fragments.
	out := append([]byte(nil), frag...)
	for {
		frag, err = r.ReadSlice('\n')
		out = append(out, frag...)
		if err == nil {
			return trimCRLF(out), nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

func trimCRLF(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
		if len(b) > 0 && b[len(b)-1] == '\r' {
			b = b[:len(b)-1]
		}
	}
	return b
}

// headerNameEq reports whether the raw header name equals s,
// case-insensitively, without allocating. equalFoldBytes does the same
// for a header value. atoiBytes parses a non-negative decimal from bytes.
func headerNameEq(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

func equalFoldBytes(b []byte, s string) bool { return headerNameEq(b, s) }

func atoiBytes(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// containsFoldBytes reports whether b contains sub, case-insensitively.
func containsFoldBytes(b []byte, sub string) bool {
	if len(sub) == 0 || len(b) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c := b[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// parseRequest reads one request (headers + body) from r.
// Header names are matched on the raw bytes (no lowercasing copy) and
// only the values the forge keeps are converted to strings, so a
// request with no interesting headers allocates nothing here.
func parseRequest(r *bufio.Reader) (*request, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		// A stray blank line before a request (some clients send one):
		// read the real request line.
		line, err = readLine(r)
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			return nil, errors.New("empty request line")
		}
	}
	// METHOD SP TARGET SP VERSION
	sp1 := indexByte(line, ' ')
	if sp1 < 0 {
		return nil, errors.New("bad request line")
	}
	sp2 := indexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return nil, errors.New("bad request line")
	}
	sp2 += sp1 + 1
	req := &request{
		method:        string(line[:sp1]),
		rawPath:       string(line[sp1+1 : sp2]),
		version:       string(line[sp2+1:]),
		contentLength: -1,
	}
	// Split target into path and query.
	target := req.rawPath
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.path = target[:q]
		req.query = target[q+1:]
	} else {
		req.path = target
	}
	req.path = decodePath(req.path)
	// Headers.
	for {
		hline, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if len(hline) == 0 {
			break
		}
		colon := indexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := hline[:colon]
		value := hline[colon+1:]
		for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
			value = value[1:]
		}
		switch {
		case headerNameEq(name, "authorization"):
			req.auth = string(value)
		case headerNameEq(name, "content-length"):
			if n, ok := atoiBytes(value); ok && n >= 0 {
				req.contentLength = n
			}
		case headerNameEq(name, "transfer-encoding"):
			if containsFoldBytes(value, "chunked") {
				req.chunked = true
			}
		case headerNameEq(name, "connection"):
			if containsFoldBytes(value, "close") {
				req.close = true
			}
		case headerNameEq(name, "git-protocol"):
			req.gitProto = string(value)
		case headerNameEq(name, "content-type"):
			req.ctype = string(value)
		case headerNameEq(name, "host"):
			req.host = string(value)
		case headerNameEq(name, "expect"):
			if equalFoldBytes(value, "100-continue") {
				req.expectContinue = true
			}
		}
	}
	if req.version == "HTTP/1.0" && !req.close {
		// HTTP/1.0 closes unless keep-alive was asked; the client here
		// always speaks 1.1, so treat 1.0 as close.
		req.close = true
	}
	return req, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// decodePath percent-decodes a path; the common case (no '%') copies nothing.
func decodePath(p string) string {
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) {
			if v, err := strconv.ParseUint(p[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// readBody fills req.body (Content-Length or chunked). w is the connection,
// used only to send a 100-continue interim response.
func readBody(r *bufio.Reader, c net.Conn, req *request) error {
	if req.expectContinue {
		if _, err := c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return err
		}
	}
	if req.chunked {
		var out []byte
		for {
			line, err := readLine(r)
			if err != nil {
				return err
			}
			// Chunk size may carry extensions after ';'.
			if i := indexByte(line, ';'); i >= 0 {
				line = line[:i]
			}
			n, err := strconv.ParseUint(strings.TrimSpace(string(line)), 16, 32)
			if err != nil {
				return errors.New("bad chunk size")
			}
			if n == 0 {
				// Trailers until an empty line.
				for {
					tl, err := readLine(r)
					if err != nil {
						return err
					}
					if len(tl) == 0 {
						break
					}
				}
				req.body = out
				return nil
			}
			chunk := make([]byte, int(n)+2) // data + CRLF
			if _, err := io.ReadFull(r, chunk); err != nil {
				return err
			}
			out = append(out, chunk[:n]...)
		}
	}
	if req.contentLength > 0 {
		buf := make([]byte, req.contentLength)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		req.body = buf
		return nil
	}
	req.body = nil
	return nil
}

// serveConn handles one connection until it closes or errors.
func serveConn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 32768)
	scratch := make([]byte, 0, 65536)
	for {
		req, err := parseRequest(r)
		if err != nil {
			return
		}
		if err := readBody(r, c, req); err != nil {
			return
		}
		resp := dispatch(req)
		if len(resp.body) > 32768 {
			// Large body (a clone pack): write the header and the body
			// separately instead of copying the body into the scratch
			// buffer first.
			scratch = serializeHeader(scratch[:0], req, resp)
			if _, err := c.Write(scratch); err != nil {
				return
			}
			if _, err := c.Write(resp.body); err != nil {
				return
			}
		} else {
			scratch = serialize(scratch[:0], req, resp)
			if _, err := c.Write(scratch); err != nil {
				return
			}
		}
		if req.close {
			return
		}
	}
}

func serveListener(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveConn(conn)
	}
}
