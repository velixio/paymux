package web_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/web"
)

type reportEnv struct {
	h      http.Handler
	buf    *bytes.Buffer
	mu     sync.Mutex
	counts map[string]int
	now    time.Time
}

func newReportEnv() *reportEnv {
	e := &reportEnv{buf: &bytes.Buffer{}, counts: map[string]int{}, now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	e.h = web.NewCSPReports(web.CSPReportOptions{
		Logger: slog.New(slog.NewTextHandler(e.buf, nil)),
		Count:  func(d string) { e.mu.Lock(); e.counts[d]++; e.mu.Unlock() },
		Now:    func() time.Time { return e.now },
	})
	return e
}

func (e *reportEnv) post(ip, body string) int {
	r := httptest.NewRequest("POST", "/csp-report", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/csp-report")
	r.RemoteAddr = ip + ":4000"
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w.Code
}

const sampleReport = `{"csp-report":{"document-uri":"https://pay.example.com/pay/pay_1?t=SECRETTOKEN","violated-directive":"script-src-elem",` +
	`"effective-directive":"script-src-elem","blocked-uri":"https://evil.example.org/x.js?token=abc123","original-policy":"default-src 'self'; report-uri /csp-report"}}`

func TestCSPReportIsCountedAndLoggedWithoutQueryStrings(t *testing.T) {
	e := newReportEnv()
	if code := e.post("192.0.2.1", sampleReport); code != 204 {
		t.Fatalf("status = %d", code)
	}
	if e.counts["script-src-elem"] != 1 {
		t.Errorf("counts = %v", e.counts)
	}
	out := e.buf.String()
	if !strings.Contains(out, "script-src-elem") || !strings.Contains(out, "https://evil.example.org") {
		t.Errorf("log lacks the directive or the blocked origin: %s", out)
	}
	for _, leak := range []string{"SECRETTOKEN", "abc123", "x.js", "pay_1", "token="} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
}

func TestCSPReportKeywordsAndJunk(t *testing.T) {
	e := newReportEnv()
	// inline/eval blocks have no URL; the keyword is kept.
	if code := e.post("192.0.2.1", `{"csp-report":{"effective-directive":"script-src","blocked-uri":"inline"}}`); code != 204 {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(e.buf.String(), "inline") || e.counts["script-src"] != 1 {
		t.Errorf("log %s counts %v", e.buf.String(), e.counts)
	}
	// Garbage and unknown directives are answered the same and counted as "other" at most.
	for _, body := range []string{`not json`, `{}`, `{"csp-report":{"effective-directive":"x; evil","blocked-uri":"https://a.example.com/?q=1"}}`} {
		if code := e.post("192.0.2.2", body); code != 204 {
			t.Errorf("%q: status %d", body, code)
		}
	}
	for d := range e.counts {
		if d != "script-src" && d != "other" {
			t.Errorf("unbounded metric label %q", d)
		}
	}
}

func TestCSPReportBodyIsCappedAt4KB(t *testing.T) {
	e := newReportEnv()
	big := `{"csp-report":{"effective-directive":"script-src","blocked-uri":"` + strings.Repeat("a", 5000) + `"}}`
	if code := e.post("192.0.2.1", big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("5 KB body: %d", code)
	}
	if len(e.counts) != 0 {
		t.Errorf("an oversized report was counted: %v", e.counts)
	}
	ok := `{"csp-report":{"effective-directive":"script-src","blocked-uri":"https://a.example.com/` + strings.Repeat("a", 3000) + `"}}`
	if code := e.post("192.0.2.1", ok); code != 204 {
		t.Errorf("3 KB body: %d", code)
	}
}

func TestCSPReportIsRateLimitedPerIP(t *testing.T) {
	e := newReportEnv()
	var ok, limited int
	for i := 0; i < 100; i++ {
		switch e.post("192.0.2.1", sampleReport) {
		case 204:
			ok++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	if ok == 0 || limited == 0 || ok > 30 {
		t.Fatalf("ok=%d limited=%d", ok, limited)
	}
	if code := e.post("192.0.2.9", sampleReport); code != 204 {
		t.Errorf("another address is limited too: %d", code)
	}
	e.now = e.now.Add(2 * time.Minute)
	if code := e.post("192.0.2.1", sampleReport); code != 204 {
		t.Errorf("the window never reopens: %d", code)
	}
	if e.counts["script-src-elem"] != ok+2 {
		t.Errorf("limited reports were counted: %v (ok=%d)", e.counts, ok)
	}
}

func TestCSPReportOnlyAcceptsPOST(t *testing.T) {
	e := newReportEnv()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, httptest.NewRequest("GET", "/csp-report", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}
}
