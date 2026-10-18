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
//
// It is not safe to store as it is. What the client strips before handing it
// over: the URL's user info; every header except a short allowlist (content
// type and length, user agent, date, retry-after, and request-id style
// headers); the values of query parameters whose name looks like a credential
// (key, secret, token, signature, hash, salt, code, state, t, ...); and the
// literal HTTPOptions.Secrets wherever they occur. The bodies are the first
// CallBodyLimit bytes as sent and received and are otherwise raw, and a
// credential the client does not know by name or value can remain in the path
// or a body. The code that stores a record must therefore run all of it
// through a redactor: that is the last line of defence, not an extra.
type CallRecord struct {
	Method            string
	URL               url.URL // without user info
	Status            int     // 0 when no response arrived
	Duration          time.Duration
	Failure           CallFailure
	Err               error       // the transport or decode error
	Attempt           int         // 1 for the first attempt of one call
	WillRetry         bool        // the client sends the request again
	RequestHeader     http.Header // allowlisted headers only
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
	scrub     func(string) string
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
		RequestHeader: safeHeader(s.req.Header, s.scrub), ResponseHeader: safeHeader(s.respHdr, s.scrub),
		ResponseBody: []byte(s.scrub(s.respBuf.b.String())), ResponseTruncated: s.respBuf.truncated,
		RequestBody: []byte(s.scrub(s.reqBuf.b.String())), RequestTruncated: s.reqBuf.truncated,
	}
	if c.Err == nil {
		c.Err = s.decodeErr
	}
	s.mu.Unlock()
	c.URL.User = nil
	c.URL.Path, c.URL.RawPath = s.scrub(c.URL.Path), ""
	c.URL.RawQuery = redactQuery(c.URL.RawQuery, s.scrub)
	c.URL.Fragment = ""
	if s.getBody != nil {
		if rc, err := s.getBody(); err == nil {
			var b capBuf
			_, _ = io.Copy(&capWriter{&b}, rc)
			rc.Close()
			c.RequestBody, c.RequestTruncated = []byte(s.scrub(b.b.String())), b.truncated
		}
	}
	defer func() { _ = recover() }() // a recorder bug must not break the call
	s.rec(s.ctx, c)
}

type capWriter struct{ b *capBuf }

func (w *capWriter) Write(p []byte) (int, error) { w.b.write(p); return len(p), nil }

// allowHeader says which headers a CallRecord may carry. Everything else,
// including any custom credential header a provider invents (x-api-key,
// x-client-secret, x-amz-security-token, signatures, sessions), is dropped.
func allowHeader(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "content-type", "content-length", "user-agent", "accept", "date", "retry-after", "cf-ray", "x-amzn-requestid":
		return true
	}
	return strings.Contains(n, "request-id") || strings.Contains(n, "request_id") || strings.Contains(n, "correlation-id") ||
		strings.Contains(n, "trace-id")
}

// safeHeader copies the allowlisted headers of h, with the client's literal
// secrets scrubbed from their values.
func safeHeader(h http.Header, scrub func(string) string) http.Header {
	out := http.Header{}
	for k, vs := range h {
		if !allowHeader(k) {
			continue
		}
		for _, v := range vs {
			out.Add(k, scrub(v))
		}
	}
	return out
}

// queryExact are short query parameter names that are credentials.
var queryExact = map[string]bool{"t": true, "cs": true, "code": true, "state": true, "otp": true, "pin": true, "salt": true,
	"nonce": true, "sid": true, "session": true}

// redactQuery replaces the value of every credential-looking parameter.
func redactQuery(raw string, scrub func(string) string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return scrub(raw)
	}
	for k, vs := range q {
		for i := range vs {
			if queryExact[strings.ToLower(k)] || secretName(k) {
				vs[i] = "xxxxx"
			} else {
				vs[i] = scrub(vs[i])
			}
		}
	}
	return q.Encode()
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
