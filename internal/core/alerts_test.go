package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
)

// alertRec records the ops alerts the service raises.
type alertRec struct {
	mu     sync.Mutex
	alerts []notify.Alert
	fail   bool
}

func (a *alertRec) Raise(_ context.Context, al notify.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("no channel")
	}
	a.alerts = append(a.alerts, al)
	return nil
}

func (a *alertRec) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, x := range a.alerts {
		out = append(out, x.Kind)
	}
	return out
}

func (a *alertRec) last() notify.Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.alerts[len(a.alerts)-1]
}

func TestDuplicateSuccessRaisesAnOpsAlertInReviewMode(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		if len(e.alerts.kinds()) != 0 {
			t.Fatalf("a normal success alerts: %v", e.alerts.kinds())
		}
		e.apply(a2.ID, captured(500))
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"payment.duplicate_success"}) {
			t.Fatalf("alerts %v", k)
		}
		al := e.alerts.last()
		if al.Severity != notify.SeverityWarning || al.Refs["payment"] != p.ID || al.Refs["attempt"] != a2.ID || !strings.Contains(al.Message, "refund") {
			t.Errorf("alert %+v", al)
		}
		e.apply(a2.ID, captured(500)) // replay
		if len(e.alerts.kinds()) != 1 {
			t.Errorf("replay alerted again: %v", e.alerts.kinds())
		}
	})
}

func TestDuplicateSuccessAutoRefundRaisesAnInfoAlert(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "duplicate_payment_action", json.RawMessage(`"auto_refund"`)); err != nil {
			t.Fatal(err)
		}
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		e.apply(a2.ID, captured(500))
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"payment.duplicate_refunded"}) || e.alerts.last().Severity != notify.SeverityInfo {
			t.Errorf("alerts %v", e.alerts.kinds())
		}
	})
}

func TestNeedsReviewRaisesAlerts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		e.apply(e.attempt(p.ID).ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 499, Currency: "INR"})
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"payment.needs_review"}) || e.alerts.last().Severity != notify.SeverityWarning ||
			!strings.Contains(e.alerts.last().Message, "499") {
			t.Fatalf("mismatch alert: %v %+v", e.alerts.kinds(), e.alerts.last())
		}
		c := e.payment(500)
		a := e.attempt(c.ID)
		if _, err := e.svc.CancelPayment(ctx, e.scope, audit.System, c.ID); err != nil {
			t.Fatal(err)
		}
		e.apply(a.ID, captured(500))
		if k := e.alerts.kinds(); len(k) != 2 || e.alerts.last().Severity != notify.SeverityCritical || e.alerts.last().Refs["payment"] != c.ID {
			t.Errorf("success after cancel: %v %+v", k, e.alerts.last())
		}
	})
}

func TestAlertsAreSentOnlyAfterCommitAndFailuresDoNotBreakPayments(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// A rolled-back transition raises nothing.
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		e.failEmit = true
		if _, err := e.svc.ApplyAttemptSnapshot(ctx, a2.ID, captured(500)); err == nil {
			t.Fatal("expected the emitter failure")
		}
		e.failEmit = false
		if len(e.alerts.kinds()) != 0 {
			t.Errorf("alert raised for a rolled-back change: %v", e.alerts.kinds())
		}
		// An alert channel that is down does not undo the payment.
		e.alerts.fail = true
		r, err := e.svc.ApplyAttemptSnapshot(ctx, a2.ID, captured(500))
		if err != nil || !r.Payment.HasFlag("duplicate_success") {
			t.Errorf("alert failure leaked: %v %+v", err, r.Payment)
		}
	})
}
