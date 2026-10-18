package connector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CallBodyLimit is the most of a request or response body a CallRecord carries.
const CallBodyLimit = 64 << 10

// CallFailure says why a provider call is being reported.
type CallFailure string

// The reasons a call is recorded. A successful call is never recorded.
const (
	FailTransport CallFailure = "transport" // no response: connection refused, reset, DNS
	FailTimeout   CallFailure = "timeout"   // the attempt timed out
	FailStatus    CallFailure = "status"    // a response with a status outside 2xx
	FailDecode    CallFailure = "decode"    // a 2xx response the connector could not decode (ReportDecodeError)
)

// CallRecord describes one failed HTTP attempt to a provider, for debugging.
// It is raw: bodies are the first CallBodyLimit bytes as sent and received,
// so whoever stores it must redact it first. Credential headers
// (Authorization, Cookie, Set-Cookie, Proxy-Authorization) are never carried.
type CallRecord struct {
	Method            string
	URL               url.URL // without user info
	Status            int     // 0 when no response arrived
	Duration          time.Duration
	Failure           CallFailure
	Err               error // the transport or decode error
	Attempt           int   // 1 for the first attempt of one call
	WillRetry         bool  // the client sends the request again
	RequestHeader     http.Header
	ResponseHeader    http.Header
	RequestBody       []byte
	ResponseBody      []byte
	RequestTruncated  bool
	ResponseTruncated bool
}

// CallRecorder receives every failed attempt of a client built by
// NewHTTPClient (HTTPOptions.Recorder), with the context of the request, so a
// recorder can read the identifiers the caller put there. It runs on the
// caller's goroutine when the response body is closed (or at once when there
// is no response) and must not block. A panic in it is swallowed.
type CallRecorder func(ctx context.Context, c CallRecord)

// ReportDecodeError tells the client that built resp that the connector could
// not decode its (2xx) body, so the call is recorded as a failure. Call it
// with the response whose body failed to parse; it is a no-op for any other
// response, and reports a call only once.
func ReportDecodeError(resp *http.Response, err error) {
	if resp == nil || err == nil {
		return
	}
	if cb, ok := resp.Body.(*cancelBody); ok {
		if c, ok := cb.ReadCloser.(*captureBody); ok {
			c.st.decodeFailed(err)
		}
	}
}

// capBuf keeps the first CallBodyLimit bytes written to it.
type capBuf struct {
	b         bytes.Buffer
	truncated bool
}

func (c *capBuf) write(p []byte) {
	room := CallBodyLimit - c.b.Len()
	if room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	if len(p) > room {
		c.truncated = true
	}
}

// teeBody copies what is read from a request body into a capBuf.
type teeBody struct {
	io.ReadCloser
	buf *capBuf
	mu  *sync.Mutex
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.buf.write(p[:n])
		t.mu.Unlock()
	}
	return n, err
}

// callState is what is known about one attempt, until it is reported.
type callState struct {
	rec       CallRecorder
	ctx       context.Context
	req       *http.Request // the attempt's request
	getBody   func() (io.ReadCloser, error)
	attempt   int
	start     time.Time
	took      time.Duration
	status    int
	respHdr   http.Header
	err       error // transport error
	timedOut  bool
	decodeErr error

	mu       sync.Mutex
	reqBuf   capBuf
	respBuf  capBuf
	done     bool // the response body has been read to the end or closed
	reported bool
	willRtry bool
}

// failure reports why the attempt failed, or "" if it did not.
func (s *callState) failure() CallFailure {
	switch {
	case s.err != nil && s.timedOut:
		return FailTimeout
	case s.err != nil:
		return FailTransport
	case s.status < 200 || s.status >= 300:
		return FailStatus
	case s.decodeErr != nil:
		return FailDecode
	}
	return ""
}

func (s *callState) markRetry() {
	s.mu.Lock()
	s.willRtry = true
	s.mu.Unlock()
}

func (s *callState) finish() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	s.emit()
}

func (s *callState) decodeFailed(err error) {
	s.mu.Lock()
	if s.decodeErr == nil {
		s.decodeErr = err
	}
	done := s.done
	s.mu.Unlock()
	if done {
		s.emit() // the body was closed before the connector tried to decode it
	}
}

// emit reports the attempt once, if it failed.
func (s *callState) emit() {
	s.mu.Lock()
	if s.reported || s.failure() == "" {
		s.mu.Unlock()
		return
	}
	s.reported = true
	c := CallRecord{
		Method: s.req.Method, URL: *s.req.URL, Status: s.status, Duration: s.took, Failure: s.failure(),
		Err: s.err, Attempt: s.attempt, WillRetry: s.willRtry,
		RequestHeader: safeHeader(s.req.Header), ResponseHeader: safeHeader(s.respHdr),
		ResponseBody: append([]byte(nil), s.respBuf.b.Bytes()...), ResponseTruncated: s.respBuf.truncated,
		RequestBody: append([]byte(nil), s.reqBuf.b.Bytes()...), RequestTruncated: s.reqBuf.truncated,
	}
	if c.Err == nil {
		c.Err = s.decodeErr
	}
	s.mu.Unlock()
	c.URL.User = nil
	if s.getBody != nil {
		if rc, err := s.getBody(); err == nil {
			var b capBuf
			_, _ = io.Copy(&capWriter{&b}, rc)
			rc.Close()
			c.RequestBody, c.RequestTruncated = append([]byte(nil), b.b.Bytes()...), b.truncated
		}
	}
	defer func() { _ = recover() }() // a recorder bug must not break the call
	s.rec(s.ctx, c)
}

type capWriter struct{ b *capBuf }

func (w *capWriter) Write(p []byte) (int, error) { w.b.write(p); return len(p), nil }

// safeHeader copies h without the headers that carry credentials.
func safeHeader(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie":
			continue
		}
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// captureBody records the response body while the connector reads it, and
// reports the attempt when it has been read or closed.
type captureBody struct {
	io.ReadCloser
	st *callState
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		c.st.mu.Lock()
		c.st.respBuf.write(p[:n])
		c.st.mu.Unlock()
	}
	if errors.Is(err, io.EOF) {
		c.st.finish()
	}
	return n, err
}

func (c *captureBody) Close() error {
	err := c.ReadCloser.Close()
	c.st.finish()
	return err
}
