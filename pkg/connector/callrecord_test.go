package connector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type calls struct {
	mu   sync.Mutex
	list []CallRecord
}

func (c *calls) rec(_ context.Context, r CallRecord) {
	c.mu.Lock()
	c.list = append(c.list, r)
	c.mu.Unlock()
}

func (c *calls) all() []CallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]CallRecord(nil), c.list...)
}

func recClient(c *calls, o HTTPOptions) *http.Client {
	o.Recorder = c.rec
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Second
	}
	if o.Backoff == 0 {
		o.Backoff = time.Millisecond
	}
	return NewHTTPClient(o)
}

func drain(t *testing.T, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestRecorderSkipsSuccessfulCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer srv.Close()
	var c calls
	resp, err := recClient(&c, HTTPOptions{}).Post(srv.URL, "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
	if n := len(c.all()); n != 0 {
		t.Errorf("a successful call was recorded: %+v", c.all())
	}
}

func TestRecorderCapturesAFailedStatusWithBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "prov_req_9")
		w.Header().Set("Set-Cookie", "sid=1")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad amount"}`))
	}))
	defer srv.Close()
	var c calls
	req, _ := http.NewRequest("POST", srv.URL+"/v1/orders?x=1", strings.NewReader(`{"amount":100}`))
	req.Header.Set("Authorization", "Basic c2VjcmV0")
	req.Header.Set("Content-Type", "application/json")
	resp, err := recClient(&c, HTTPOptions{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
	got := c.all()
	if len(got) != 1 {
		t.Fatalf("records = %d", len(got))
	}
	r := got[0]
	if r.Failure != FailStatus || r.Status != 400 || r.Method != "POST" || r.URL.Path != "/v1/orders" || r.URL.RawQuery != "x=1" {
		t.Errorf("%+v", r)
	}
	if string(r.RequestBody) != `{"amount":100}` || string(r.ResponseBody) != `{"error":"bad amount"}` {
		t.Errorf("bodies: %q %q", r.RequestBody, r.ResponseBody)
	}
	if r.ResponseHeader.Get("X-Request-Id") != "prov_req_9" || r.RequestHeader.Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v %v", r.RequestHeader, r.ResponseHeader)
	}
	for _, h := range []string{"Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization"} {
		if r.RequestHeader.Get(h) != "" || r.ResponseHeader.Get(h) != "" {
			t.Errorf("%s reaches the recorder", h)
		}
	}
	if r.Duration <= 0 {
		t.Error("no duration")
	}
}

func TestRecorderCapturesTransportErrorsAndTimeouts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	var c calls
	_, err := recClient(&c, HTTPOptions{Timeout: 50 * time.Millisecond}).Get(srv.URL)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	srv.CloseClientConnections()
	srv.Close()
	_, err = recClient(&c, HTTPOptions{}).Get("http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	got := c.all()
	if len(got) != 2 || got[0].Failure != FailTimeout || got[1].Failure != FailTransport || got[1].Err == nil || got[1].Status != 0 {
		t.Errorf("%+v", got)
	}
}

func TestRecorderCapsBodies(t *testing.T) {
	big := strings.Repeat("x", CallBodyLimit*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()
	var c calls
	resp, err := recClient(&c, HTTPOptions{}).Post(srv.URL, "text/plain", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
	r := c.all()[0]
	if len(r.RequestBody) != CallBodyLimit || len(r.ResponseBody) != CallBodyLimit || !r.RequestTruncated || !r.ResponseTruncated {
		t.Errorf("sizes %d %d trunc %v %v", len(r.RequestBody), len(r.ResponseBody), r.RequestTruncated, r.ResponseTruncated)
	}
}

func TestRecorderRequestBodyWithoutGetBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); w.WriteHeader(422) }))
	defer srv.Close()
	var c calls
	req, _ := http.NewRequest("POST", srv.URL, io.NopCloser(strings.NewReader("streamed body")))
	resp, err := recClient(&c, HTTPOptions{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
	if r := c.all()[0]; string(r.RequestBody) != "streamed body" {
		t.Errorf("%q", r.RequestBody)
	}
}

func TestRecorderReportsRetriesAsWillRetry(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n++; n < 3 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("fine"))
	}))
	defer srv.Close()
	var c calls
	resp, err := recClient(&c, HTTPOptions{MaxRetries: 2}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
	got := c.all()
	if len(got) != 2 || !got[0].WillRetry || !got[1].WillRetry || got[0].Attempt != 1 || got[1].Attempt != 2 || got[0].Status != 503 {
		t.Errorf("%+v", got)
	}
}

func TestReportDecodeErrorRecordsASuccessfulStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>not json`)) }))
	defer srv.Close()
	for name, afterClose := range map[string]bool{"before close": false, "after close": true} {
		var c calls
		resp, err := recClient(&c, HTTPOptions{}).Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		if afterClose {
			resp.Body.Close()
			ReportDecodeError(resp, errors.New("invalid character '<'"))
		} else {
			ReportDecodeError(resp, errors.New("invalid character '<'"))
			resp.Body.Close()
		}
		got := c.all()
		if len(got) != 1 || got[0].Failure != FailDecode || got[0].Status != 200 || string(got[0].ResponseBody) != string(raw) || got[0].Err == nil {
			t.Errorf("%s: %+v", name, got)
		}
		ReportDecodeError(resp, errors.New("again")) // reported once
		if len(c.all()) != 1 {
			t.Errorf("%s: recorded twice", name)
		}
	}
	ReportDecodeError(nil, errors.New("x")) // no panic
	ReportDecodeError(&http.Response{Body: http.NoBody}, errors.New("x"))
}

func TestRecorderNilIsFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	resp, err := NewHTTPClient(HTTPOptions{}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
}

func TestRecorderPanicDoesNotBreakTheCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	o := HTTPOptions{Recorder: func(context.Context, CallRecord) { panic("recorder bug") }}
	resp, err := NewHTTPClient(o).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
}

func TestRecorderGetsTheRequestContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	type key struct{}
	var seen any
	o := HTTPOptions{Recorder: func(ctx context.Context, _ CallRecord) { seen = ctx.Value(key{}) }}
	req, _ := http.NewRequestWithContext(context.WithValue(context.Background(), key{}, "ids"), "GET", srv.URL, nil)
	resp, err := NewHTTPClient(o).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, resp)
	if seen != "ids" {
		t.Errorf("ctx value = %v", seen)
	}
}
