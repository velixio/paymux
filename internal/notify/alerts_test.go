package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recMailer struct {
	mu   sync.Mutex
	got  []Message
	fail error
}

func (r *recMailer) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.got = append(r.got, m)
	return nil
}

func (r *recMailer) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) }

type hookSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	bods []string
	ct   []string
	code int
}

func newHook(t *testing.T) *hookSink {
	h := &hookSink{code: 200}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bods, h.ct = append(h.bods, string(b)), append(h.ct, r.Header.Get("Content-Type"))
		c := h.code
		h.mu.Unlock()
		w.WriteHeader(c)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookSink) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.bods) }

type tclock struct{ t time.Time }

func (c *tclock) now() time.Time { return c.t }

func newAlerts(m Mailer, hook string, clk *tclock) *Alerts {
	return NewAlerts(AlertsOptions{Mailer: m, Emails: []string{"ops@example.com", "oncall@example.com"}, WebhookURL: hook, Now: clk.now})
}

func TestRaiseEmailsAndPostsSlackJSON(t *testing.T) {
	mail, hook := &recMailer{}, newHook(t)
	a := newAlerts(mail, hook.srv.URL, &tclock{time.Unix(1_800_000_000, 0)})
	err := a.Raise(context.Background(), Alert{Severity: SeverityCritical, Kind: "provider.silent",
		Message: "No verified webhooks from pa_1 in 6h", Refs: map[string]string{"provider_account": "pa_1", "app": "app_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if mail.count() != 1 {
		t.Fatalf("emails=%d", mail.count())
	}
	m := mail.got[0]
	if strings.Join(m.To, ",") != "ops@example.com,oncall@example.com" || !strings.Contains(m.Subject, "critical") ||
		!strings.Contains(m.Subject, "provider.silent") || !strings.Contains(m.Text, "No verified webhooks from pa_1 in 6h") ||
		!strings.Contains(m.Text, "provider_account=pa_1") {
		t.Errorf("email %+v", m)
	}
	if hook.count() != 1 || hook.ct[0] != "application/json" {
		t.Fatalf("webhook calls=%d ct=%v", hook.count(), hook.ct)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(hook.bods[0]), &body); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"CRITICAL", "provider.silent", "No verified webhooks from pa_1 in 6h", "app=app_1"} {
		if !strings.Contains(body.Text, w) {
			t.Errorf("webhook text %q missing %q", body.Text, w)
		}
	}
}

func TestIdenticalAlertsRateLimitedForTenMinutes(t *testing.T) {
	mail, hook := &recMailer{}, newHook(t)
	clk := &tclock{time.Unix(1_800_000_000, 0)}
	a := newAlerts(mail, hook.srv.URL, clk)
	al := Alert{Severity: SeverityWarning, Kind: "k", Message: "m", Refs: map[string]string{"a": "1", "b": "2"}}
	raise := func(x Alert) {
		t.Helper()
		if err := a.Raise(context.Background(), x); err != nil {
			t.Fatal(err)
		}
	}
	raise(al)
	clk.t = clk.t.Add(9*time.Minute + 59*time.Second)
	raise(Alert{Severity: SeverityWarning, Kind: "k", Message: "m", Refs: map[string]string{"b": "2", "a": "1"}}) // same, map order irrelevant
	if mail.count() != 1 || hook.count() != 1 {
		t.Fatalf("duplicate within window sent: mail=%d hook=%d", mail.count(), hook.count())
	}
	raise(Alert{Severity: SeverityWarning, Kind: "k", Message: "different"})
	raise(Alert{Severity: SeverityCritical, Kind: "k", Message: "m", Refs: al.Refs})
	if mail.count() != 3 {
		t.Fatalf("distinct alerts must not be suppressed: %d", mail.count())
	}
	clk.t = clk.t.Add(2 * time.Second) // 10m01s after the first
	raise(al)
	if mail.count() != 4 || hook.count() != 4 {
		t.Fatalf("after the window: mail=%d hook=%d", mail.count(), hook.count())
	}
}

func TestFailedDeliveryIsNotRateLimitedAndReportsError(t *testing.T) {
	mail, hook := &recMailer{fail: errors.New("smtp down")}, newHook(t)
	hook.code = 500
	a := newAlerts(mail, hook.srv.URL, &tclock{time.Unix(1_800_000_000, 0)})
	al := Alert{Severity: SeverityCritical, Kind: "k", Message: "m"}
	err := a.Raise(context.Background(), al)
	if err == nil || !strings.Contains(err.Error(), "smtp down") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err=%v", err)
	}
	mail.fail = nil
	hook.code = 200
	if err := a.Raise(context.Background(), al); err != nil || mail.count() != 1 {
		t.Fatalf("retry after total failure suppressed: err=%v mails=%d", err, mail.count())
	}
}

func TestOneChannelFailingStillCountsAsSent(t *testing.T) {
	mail, hook := &recMailer{fail: errors.New("smtp down")}, newHook(t)
	a := newAlerts(mail, hook.srv.URL, &tclock{time.Unix(1_800_000_000, 0)})
	al := Alert{Severity: SeverityInfo, Kind: "k", Message: "m"}
	if err := a.Raise(context.Background(), al); err == nil {
		t.Fatal("partial failure must be reported")
	}
	if err := a.Raise(context.Background(), al); err != nil || hook.count() != 1 {
		t.Fatalf("second raise should be rate limited: err=%v hook=%d", err, hook.count())
	}
}

func TestNoChannelsConfiguredIsANoOp(t *testing.T) {
	a := NewAlerts(AlertsOptions{})
	if err := a.Raise(context.Background(), Alert{Kind: "k", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	// emails without recipients, webhook without URL
	mail := &recMailer{}
	a = NewAlerts(AlertsOptions{Mailer: mail})
	_ = a.Raise(context.Background(), Alert{Kind: "k", Message: "m"})
	if mail.count() != 0 {
		t.Fatal("emailed nobody")
	}
}

func TestRateLimitMemoryIsBounded(t *testing.T) {
	clk := &tclock{time.Unix(1_800_000_000, 0)}
	a := NewAlerts(AlertsOptions{Mailer: &recMailer{}, Emails: []string{"o@example.com"}, Now: clk.now})
	for i := 0; i < 50; i++ {
		_ = a.Raise(context.Background(), Alert{Kind: "k", Message: strings.Repeat("x", i+1)})
	}
	clk.t = clk.t.Add(11 * time.Minute)
	_ = a.Raise(context.Background(), Alert{Kind: "k", Message: "trigger prune"})
	if n := a.tracked(); n != 1 {
		t.Fatalf("expired entries kept: %d", n)
	}
}
