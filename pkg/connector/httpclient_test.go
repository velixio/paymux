package connector

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testOpts(buf *bytes.Buffer) HTTPOptions {
	return HTTPOptions{
		Timeout:    2 * time.Second,
		MaxRetries: 2,
		Backoff:    time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(buf, nil)),
	}
}

func TestHTTPClientRetriesGETOn5xx(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(502)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := NewHTTPClient(testOpts(&bytes.Buffer{}))
	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts = %d", n.Load())
	}
}

func TestHTTPClientGivesUpAfterMaxRetries(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	c := NewHTTPClient(testOpts(&bytes.Buffer{}))
	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if n.Load() != 3 { // 1 + 2 retries
		t.Fatalf("attempts = %d", n.Load())
	}
}

func TestHTTPClientDoesNotRetryPOSTOr4xx(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := NewHTTPClient(testOpts(&bytes.Buffer{}))
	resp, err := c.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n.Load() != 1 {
		t.Fatalf("POST attempts = %d", n.Load())
	}
	n.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(404)
	}))
	defer srv2.Close()
	resp, _ = c.Get(srv2.URL)
	resp.Body.Close()
	if n.Load() != 1 {
		t.Fatalf("404 attempts = %d", n.Load())
	}
}

func TestHTTPClientRetriesTimeouts(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			time.Sleep(200 * time.Millisecond)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	o := testOpts(&bytes.Buffer{})
	o.Timeout = 50 * time.Millisecond
	c := NewHTTPClient(o)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n.Load() != 2 {
		t.Fatalf("attempts = %d", n.Load())
	}
}

func TestHTTPClientLogsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	var buf bytes.Buffer
	o := testOpts(&buf)
	o.Secrets = []string{"supersecretvalue"}
	c := NewHTTPClient(o)
	req, _ := http.NewRequest("GET", srv.URL+"/x?api_key=abc123&q=supersecretvalue&ok=1", nil)
	req.SetBasicAuth("rzp_key", "hunter2hunter2")
	req.Header.Set("Authorization", "Bearer tok_live_999")
	req.Header.Set("X-Api-Key", "headersecret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	out := buf.String()
	for _, leak := range []string{"hunter2", "tok_live_999", "abc123", "supersecretvalue", "headersecret", "rzp_key"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "GET") || !strings.Contains(out, "status=200") || !strings.Contains(out, "ok=1") {
		t.Errorf("log lacks request/response lines:\n%s", out)
	}
}

func TestHTTPClientRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	now := time.Unix(0, 0)
	var slept []time.Duration
	o := testOpts(&bytes.Buffer{})
	o.RateLimit = RateLimit{RequestsPerSecond: 10, Burst: 2}
	o.Now = func() time.Time { return now }
	o.Sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		now = now.Add(d)
		return nil
	}
	c := NewHTTPClient(o)
	for i := 0; i < 4; i++ {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	// burst of 2 is free; requests 3 and 4 each wait 100ms.
	if len(slept) != 2 || slept[0] != 100*time.Millisecond || slept[1] != 100*time.Millisecond {
		t.Fatalf("slept = %v", slept)
	}
}

func TestHTTPClientRateLimitContextCancel(t *testing.T) {
	o := testOpts(&bytes.Buffer{})
	o.RateLimit = RateLimit{RequestsPerSecond: 0.001, Burst: 1}
	c := NewHTTPClient(o)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	resp, _ := c.Get(srv.URL)
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected context error")
	}
}
