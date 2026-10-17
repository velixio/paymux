package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Severity ranks an alert.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// RateLimitWindow is how long an identical alert is suppressed.
const RateLimitWindow = 10 * time.Minute

// Alert is something the operator should know about.
type Alert struct {
	Severity Severity
	Kind     string // e.g. "provider.silent"
	Message  string
	Refs     map[string]string // related ids: payment, provider_account, ...
}

// AlertsOptions configures Alerts. Empty Emails or WebhookURL disable that
// channel.
type AlertsOptions struct {
	Mailer     Mailer
	Emails     []string // PAYMUX_OPS_EMAILS
	WebhookURL string   // PAYMUX_OPS_WEBHOOK_URL, Slack-compatible
	Client     *http.Client
	Now        func() time.Time
}

// Alerts delivers ops alerts and rate-limits identical ones, in memory (a
// restart forgets the window, which at worst repeats an alert once).
type Alerts struct {
	o AlertsOptions

	mu   sync.Mutex
	last map[string]time.Time
}

// NewAlerts builds an Alerts service.
func NewAlerts(o AlertsOptions) *Alerts {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Mailer == nil {
		o.Mailer = NoopMailer{}
	}
	return &Alerts{o: o, last: map[string]time.Time{}}
}

func (a Alert) key() string {
	keys := make([]string, 0, len(a.Refs))
	for k := range a.Refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s", a.Severity, a.Kind, a.Message)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, a.Refs[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (a Alert) refsString() string {
	keys := make([]string, 0, len(a.Refs))
	for k := range a.Refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + a.Refs[k]
	}
	return strings.Join(parts, " ")
}

// claim reports whether the alert may be sent now, and records it as sent.
// Expired entries are pruned so memory stays bounded by the alert rate.
func (s *Alerts) claim(k string, now time.Time) (ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, t := range s.last {
		if now.Sub(t) >= RateLimitWindow {
			delete(s.last, key)
		}
	}
	if _, dup := s.last[k]; dup {
		return false
	}
	s.last[k] = now
	return true
}

func (s *Alerts) release(k string) { s.mu.Lock(); delete(s.last, k); s.mu.Unlock() }

func (s *Alerts) tracked() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.last) }

// Raise sends the alert to every configured channel unless an identical one
// went out in the last 10 minutes. It returns the joined channel errors; an
// alert that reached no channel because all failed is not rate-limited, so
// the next identical Raise tries again.
func (s *Alerts) Raise(ctx context.Context, al Alert) error {
	if al.Severity == "" {
		al.Severity = SeverityWarning
	}
	k := al.key()
	if !s.claim(k, s.o.Now()) {
		return nil
	}
	var errs []error
	channels, failed := 0, 0
	if len(s.o.Emails) > 0 {
		channels++
		body := al.Message
		if r := al.refsString(); r != "" {
			body += "\n\n" + r
		}
		err := s.o.Mailer.Send(ctx, Message{To: s.o.Emails,
			Subject: fmt.Sprintf("[paymux][%s] %s", al.Severity, al.Kind), Text: body + "\n"})
		if err != nil {
			failed++
			errs = append(errs, fmt.Errorf("alert email: %w", err))
		}
	}
	if s.o.WebhookURL != "" {
		channels++
		if err := s.post(ctx, al); err != nil {
			failed++
			errs = append(errs, fmt.Errorf("alert webhook: %w", err))
		}
	}
	if channels > 0 && failed == channels {
		s.release(k)
	}
	return errors.Join(errs...)
}

func (s *Alerts) post(ctx context.Context, al Alert) error {
	text := fmt.Sprintf("[%s] %s: %s", strings.ToUpper(string(al.Severity)), al.Kind, al.Message)
	if r := al.refsString(); r != "" {
		text += " (" + r + ")"
	}
	b, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.o.WebhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.o.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}
