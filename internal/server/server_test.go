package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/metrics"
)

func newServer(t *testing.T, checks ...Check) (*Server, *metrics.Registry, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	reg := metrics.NewRegistry()
	s := New(Options{Logger: logging.New(&logs, -4), Metrics: reg, Checks: checks})
	return s, reg, &logs
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequestIDHonouredAndGenerated(t *testing.T) {
	s, _, _ := newServer(t)
	h := s.Handler()
	if got := get(h, "/healthz", "X-Request-Id", "abc-123").Header().Get("X-Request-Id"); got != "abc-123" {
		t.Errorf("honoured: %q", got)
	}
	for _, bad := range []string{"", "has space", "bad_underscore", strings.Repeat("a", 65)} {
		got := get(h, "/healthz", "X-Request-Id", bad).Header().Get("X-Request-Id")
		if !strings.HasPrefix(got, "req_") || got == bad {
			t.Errorf("bad %q -> %q", bad, got)
		}
	}
}

func TestHealthz(t *testing.T) {
	s, _, _ := newServer(t)
	rec := get(s.Handler(), "/healthz")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatal(ct)
	}
}

func TestPanicReturns500JSON(t *testing.T) {
	s, _, _ := newServer(t)
	s.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	rec := get(s.Handler(), "/boom", "X-Request-Id", "rid-1")
	if rec.Code != 500 {
		t.Fatal(rec.Code)
	}
	var body struct {
		Error struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err, rec.Body.String())
	}
	if body.Error.Type != "internal" || body.Error.RequestID != "rid-1" {
		t.Fatalf("%+v", body)
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Fatal("panic value leaked to client")
	}
}

func TestReadyzNotReadyBeforeQueue(t *testing.T) {
	queueUp := false
	s, _, _ := newServer(t,
		Check{"db", func(context.Context) error { return nil }},
		Check{"queue", func(context.Context) error {
			if !queueUp {
				return errors.New("not running")
			}
			return nil
		}},
	)
	rec := get(s.Handler(), "/readyz")
	var body struct {
		Status  string
		Failing []string
	}
	if rec.Code != 503 {
		t.Fatalf("code %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Failing) != 1 || body.Failing[0] != "queue" {
		t.Fatalf("%+v", body)
	}
	queueUp = true
	rec = get(s.Handler(), "/readyz")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ready"}` {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
}

func TestRequestMetricsAndAccessLog(t *testing.T) {
	s, reg, logs := newServer(t)
	get(s.Handler(), "/healthz")
	get(s.Handler(), "/nope")
	rec := get(s.InternalHandler(), "/metrics")
	out := rec.Body.String()
	for _, want := range []string{
		`paymux_http_requests_total{route="GET /healthz",code="200"} 1`,
		`paymux_http_requests_total{route="unmatched",code="404"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	_ = reg
	if !strings.Contains(logs.String(), `"msg":"http request"`) || !strings.Contains(logs.String(), `"request_id":"req_`) {
		t.Errorf("access log: %s", logs)
	}
}

func TestInternalPprof(t *testing.T) {
	s, _, _ := newServer(t)
	if rec := get(s.InternalHandler(), "/debug/pprof/"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := get(s.Handler(), "/debug/pprof/"); rec.Code != 404 {
		t.Fatalf("pprof exposed on main listener: %d", rec.Code)
	}
	if rec := get(s.Handler(), "/metrics"); rec.Code != 404 {
		t.Fatalf("metrics exposed on main listener: %d", rec.Code)
	}
}
