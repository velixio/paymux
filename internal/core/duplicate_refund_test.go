package core_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
)

// A customer who paid twice gets the extra payment back. That refund belongs to
// the extra attempt, not to the sale: the payment keeps refunded = 0, is not
// flagged refunded, and the app is not told its sale was refunded.
func TestRefundOfTheDuplicateAttemptDoesNotTouchTheSale(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "duplicate_payment_action", json.RawMessage(`"auto_refund"`)); err != nil {
			t.Fatal(err)
		}
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		e.apply(a2.ID, captured(500))
		var refunds []store.Refund
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { refunds, err = store.ListRefunds(ctx, q, p.ID); return })
		if len(refunds) != 1 {
			t.Fatalf("refunds %+v", refunds)
		}
		before := len(e.emitted())
		res, err := e.svc.ApplyRefundSnapshot(ctx, refunds[0].ID, core.RefundSnapshot{Status: state.RefundSucceeded, ProviderRefundID: "rf_dup"})
		if err != nil || res.Refund.Status != "succeeded" {
			t.Fatalf("%+v %v", res, err)
		}
		got, err := e.svc.GetPaymentByID(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.AmountRefunded != 0 || got.HasFlag("refunded_full") || got.HasFlag("refunded_partial") || got.HasFlag("refund_exceeds_balance") {
			t.Errorf("the sale was marked refunded: refunded=%d flags=%v", got.AmountRefunded, got.Flags)
		}
		if !got.HasFlag("duplicate_refunded") {
			t.Errorf("no record that the duplicate was refunded: %v", got.Flags)
		}
		for _, typ := range e.emitted()[before:] {
			if typ == "refund.succeeded" || typ == "payment.needs_review" {
				t.Errorf("event %s was emitted for a refund of the extra attempt", typ)
			}
		}
		if !slices.Contains(e.emitted()[before:], "payment.duplicate_refunded") {
			t.Errorf("events %v", e.emitted()[before:])
		}
		// The ledger nets the extra charge out: two charges, one refund of 500.
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'charge'") != 2 || e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'refund' AND amount = -500 AND refund_id = ?", refunds[0].ID) != 1 {
			t.Error("the duplicate refund is not in the ledger")
		}
		// A real refund of the sale still works, for the full amount.
		real, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "dashboard", PaymentID: p.ID, Amount: 500})
		if err != nil || real.AttemptID != a1.ID {
			t.Fatalf("real refund: %+v %v", real, err)
		}
		if _, err := e.svc.ApplyRefundSnapshot(ctx, real.ID, core.RefundSnapshot{Status: state.RefundSucceeded, ProviderRefundID: "rf_real"}); err != nil {
			t.Fatal(err)
		}
		got, _ = e.svc.GetPaymentByID(ctx, p.ID)
		if got.AmountRefunded != 500 || !got.HasFlag("refunded_full") || got.HasFlag("refund_exceeds_balance") {
			t.Errorf("after the real refund: refunded=%d flags=%v", got.AmountRefunded, got.Flags)
		}
	})
}

// A duplicate refunded by an operator from the dashboard is the same case.
func TestOperatorRefundOfTheDuplicateAttempt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		e.apply(a2.ID, captured(500))
		rf, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "dashboard", PaymentID: p.ID, AttemptID: a2.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ApplyRefundSnapshot(ctx, rf.ID, core.RefundSnapshot{Status: state.RefundSucceeded}); err != nil {
			t.Fatal(err)
		}
		got, _ := e.svc.GetPaymentByID(ctx, p.ID)
		if got.AmountRefunded != 0 || got.HasFlag("refunded_full") {
			t.Errorf("refunded=%d flags=%v", got.AmountRefunded, got.Flags)
		}
	})
}
