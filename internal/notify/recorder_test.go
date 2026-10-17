package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recRecorder struct {
	mu   sync.Mutex
	got  []Alert
	at   []time.Time
	fail error
	// order records what happened first when a mailer is also involved
	log *[]string
}

func (r *recRecorder) Record(_ context.Context, al Alert, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.log != nil {
		*r.log = append(*r.log, "record")
	}
	if r.fail != nil {
		return r.fail
	}
	r.got, r.at = append(r.got, al), append(r.at, at)
	return nil
}

func (r *recRecorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) }

type orderMailer struct {
	log *[]string
	recMailer
}

func (m *orderMailer) Send(ctx context.Context, msg Message) error {
	*m.log = append(*m.log, "send")
	return m.recMailer.Send(ctx, msg)
}

func TestEveryRaiseIsRecordedEvenWhenDeliveryIsRateLimited(t *testing.T) {
	mail := &recMailer{}
	rec := &recRecorder{}
	clk := &tclock{time.Unix(1_800_000_000, 0)}
	a := NewAlerts(AlertsOptions{Mailer: mail, Emails: []string{"ops@example.com"}, Now: clk.now, Recorder: rec})
	al := Alert{Severity: SeverityCritical, Kind: "provider.silent", Message: "No webhooks", Refs: map[string]string{"provider_account": "pa_1"}}
	for i := 0; i < 3; i++ {
		if err := a.Raise(context.Background(), al); err != nil {
			t.Fatal(err)
		}
	}
	if mail.count() != 1 {
		t.Errorf("delivery is unchanged: %d emails for three identical raises", mail.count())
	}
	if rec.count() != 3 {
		t.Errorf("recorded %d, want every raise (3)", rec.count())
	}
	if !rec.at[0].Equal(clk.t) {
		t.Errorf("recorded at %v, want the clock's %v", rec.at[0], clk.t)
	}
}

func TestRecordedAlertGetsADefaultSeverityAndIsRecordedBeforeDelivery(t *testing.T) {
	var log []string
	mail := &orderMailer{log: &log}
	rec := &recRecorder{log: &log}
	a := NewAlerts(AlertsOptions{Mailer: mail, Emails: []string{"ops@example.com"}, Recorder: rec})
	if err := a.Raise(context.Background(), Alert{Kind: "x", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	if rec.got[0].Severity != SeverityWarning {
		t.Errorf("severity %q", rec.got[0].Severity)
	}
	if len(log) != 2 || log[0] != "record" || log[1] != "send" {
		t.Errorf("order %v: an alert must be on record even if delivery hangs or fails", log)
	}
}

func TestARecorderFailureNeverStopsDeliveryOrFailsTheRaise(t *testing.T) {
	mail := &recMailer{}
	rec := &recRecorder{fail: errors.New("database is locked")}
	a := NewAlerts(AlertsOptions{Mailer: mail, Emails: []string{"ops@example.com"}, Recorder: rec})
	if err := a.Raise(context.Background(), Alert{Kind: "x", Message: "m"}); err != nil {
		t.Fatalf("Raise = %v: callers retry on an error, and the alert was delivered", err)
	}
	if mail.count() != 1 {
		t.Error("the email must still go out")
	}
}

func TestFingerprintIgnoresMessageAndSeverityButNotKindOrRefs(t *testing.T) {
	base := Alert{Severity: SeverityWarning, Kind: "provider.silent", Message: "quiet for 6h", Refs: map[string]string{"a": "1", "b": "2"}}
	same := Alert{Severity: SeverityCritical, Kind: "provider.silent", Message: "quiet for 7h", Refs: map[string]string{"b": "2", "a": "1"}}
	if base.Fingerprint() != same.Fingerprint() {
		t.Error("the same condition with a new message or severity is the same alert")
	}
	for name, other := range map[string]Alert{
		"kind": {Kind: "other", Refs: base.Refs},
		"refs": {Kind: base.Kind, Refs: map[string]string{"a": "1", "b": "3"}},
		"none": {Kind: base.Kind},
	} {
		if other.Fingerprint() == base.Fingerprint() {
			t.Errorf("a different %s must be a different alert", name)
		}
	}
}
