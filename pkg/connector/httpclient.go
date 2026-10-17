package connector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTPOptions configures NewHTTPClient.
type HTTPOptions struct {
	// Timeout bounds each attempt (default 15s).
	Timeout time.Duration
	// MaxRetries is the number of retries after the first attempt, applied only
	// to GET (and HEAD) requests that fail with a network error, a timeout or a
	// 5xx response.
	MaxRetries int
	// Backoff is the first retry delay; it doubles each retry (default 200ms).
	Backoff time.Duration
	// Logger receives one request line and one response line per attempt.
	Logger *slog.Logger
	// Secrets are literal strings that must never reach the logs.
	Secrets []string
	// RateLimit is a per-account token bucket; zero RequestsPerSecond disables it.
	RateLimit RateLimit
	// Observer, when set, receives one RequestObservation per attempt (latency
	// and outcome only: no URL, headers or body).
	Observer Observer
	// DenyPrivate refuses to connect to loopback, private, link-local, multicast
	// and unspecified addresses (checked for every address a name resolves to,
	// and again at connect time), ignores environment proxies and never follows
	// redirects. It replaces Base. For connectors whose target is configured by
	// an operator, such as the remote connector.
	DenyPrivate bool
	// LookupIP resolves host names when DenyPrivate is set (default: the system
	// resolver); tests use it to simulate DNS answers.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	// Base is the underlying transport (default http.DefaultTransport).
	Base http.RoundTripper
	// Now and Sleep are test seams for the limiter and the backoff.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewHTTPClient returns an *http.Client with per-attempt timeouts, retries of
// idempotent GETs, redacted request/response logging and a token-bucket rate
// limiter. Create one client per provider account so the limiter is per account.
func NewHTTPClient(o HTTPOptions) *http.Client {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.Backoff <= 0 {
		o.Backoff = 200 * time.Millisecond
	}
	if o.DenyPrivate {
		o.Base = guardedTransport(o.LookupIP)
	}
	if o.Base == nil {
		o.Base = http.DefaultTransport
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	t := &transport{o: o}
	if o.RateLimit.RequestsPerSecond > 0 {
		burst := o.RateLimit.Burst
		if burst < 1 {
			burst = 1
		}
		t.bucket = &bucket{rate: o.RateLimit.RequestsPerSecond, burst: float64(burst), tokens: float64(burst), last: o.Now()}
	}
	c := &http.Client{Transport: t}
	if o.DenyPrivate {
		c.CheckRedirect = noRedirects
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// reserve takes a token and returns how long the caller must wait for it.
func (b *bucket) reserve(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

type transport struct {
	o      HTTPOptions
	bucket *bucket
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	retryable := (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.Body == nil
	retries := 0
	if retryable {
		retries = t.o.MaxRetries
	}
	backoff := t.o.Backoff
	for attempt := 0; ; attempt++ {
		if t.bucket != nil {
			if d := t.bucket.reserve(t.o.Now()); d > 0 {
				if err := t.o.Sleep(req.Context(), d); err != nil {
					return nil, err
				}
			}
		}
		resp, err := t.once(req)
		failed := err != nil || resp.StatusCode >= 500
		if !failed || attempt >= retries || req.Context().Err() != nil {
			return resp, err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		}
		if err := t.o.Sleep(req.Context(), backoff); err != nil {
			return nil, err
		}
		backoff *= 2
	}
}

func (t *transport) once(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.o.Timeout)
	r := req.Clone(ctx)
	start := t.o.Now()
	t.logRequest(r)
	resp, err := t.o.Base.RoundTrip(r)
	if t.o.Observer != nil {
		t.o.Observer(observe(req.Method, resp, err, t.o.Now().Sub(start)))
	}
	if err != nil {
		cancel()
		t.logError(r, err)
		return nil, err
	}
	t.logResponse(r, resp, t.o.Now().Sub(start))
	resp.Body = &cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (t *transport) logRequest(r *http.Request) {
	if t.o.Logger == nil {
		return
	}
	attrs := []any{"method", r.Method, "url", t.redactURL(r.URL)}
	if r.Header.Get("Authorization") != "" || r.URL.User != nil {
		attrs = append(attrs, "auth", "[redacted]")
	}
	t.o.Logger.Info("connector http request", attrs...)
}

func (t *transport) logResponse(r *http.Request, resp *http.Response, d time.Duration) {
	if t.o.Logger == nil {
		return
	}
	t.o.Logger.Info("connector http response", "method", r.Method, "url", t.redactURL(r.URL),
		"status", resp.StatusCode, "duration_ms", d.Milliseconds())
}

func (t *transport) logError(r *http.Request, err error) {
	if t.o.Logger == nil {
		return
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	t.o.Logger.Warn("connector http error", "method", r.Method, "url", t.redactURL(r.URL), "error", t.scrub(err.Error()))
}

var secretWords = []string{"key", "secret", "token", "password", "passwd", "auth", "sig", "hash", "credential"}

func secretName(k string) bool {
	k = strings.ToLower(k)
	for _, w := range secretWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func (t *transport) redactURL(u *url.URL) string {
	c := *u
	c.User = nil
	q := c.Query()
	for k := range q {
		if secretName(k) {
			q.Set(k, "xxxxx")
		}
	}
	c.RawQuery = q.Encode()
	return t.scrub(c.String())
}

// scrub removes every configured literal secret, raw or URL-encoded.
func (t *transport) scrub(s string) string {
	for _, sec := range t.o.Secrets {
		if sec == "" {
			continue
		}
		s = strings.ReplaceAll(s, sec, "xxxxx")
		s = strings.ReplaceAll(s, url.QueryEscape(sec), "xxxxx")
	}
	return s
}
