package core_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// decline is a declined try of card last4. Each try is its own provider
// payment, as at a real provider: try only labels it in the test.
var declines int

func decline(try int, last4 string) core.Snapshot {
	declines++
	return core.Snapshot{Status: state.AttemptFailed, FailureCode: "card_declined", FailureMessage: "declined",
		ProviderPaymentID: fmt.Sprintf("pp_d%d_%d", declines, try), Method: "card",
		MethodDetails: connector.PaymentMethodDetails{Card: &connector.CardDetails{Last4: last4, Network: "visa"}}}
}

// paidWith is a capture of amount by card last4, with a risk score.
// Each capture is its own provider payment: one cannot settle two attempts.
var captures int

func paidWith(amount int64, last4 string, score int) core.Snapshot {
	captures++
	return core.Snapshot{Status: state.AttemptCaptured, Amount: amount, Currency: "INR", Method: "card", ProviderPaymentID: fmt.Sprintf("pp_paid%d", captures),
		MethodDetails: connector.PaymentMethodDetails{Card: &connector.CardDetails{Last4: last4, Network: "visa"}},
		Risk:          connector.PaymentRisk{Score: &score}}
}

func (e *env) customerPayment(amount int64, ref string) store.Payment {
	e.t.Helper()
	res, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: amount, Currency: "INR", OrderRef: "ord-1",
		Customer: &core.CustomerInput{ExternalRef: ref, Email: ref + "@example.com"}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Payment
}

func (e *env) alertKinds() []string {
	e.alerts.mu.Lock()
	defer e.alerts.mu.Unlock()
	var out []string
	for _, a := range e.alerts.alerts {
		out = append(out, a.Kind)
	}
	return out
}

func TestSuccessAfterDeclinedCardsGoesToReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// Two other cards declined: under review_after_declined_cards (3).
		p := e.payment(500)
		a := e.attempt(p.ID)
		e.apply(a.ID, decline(1, "1111"))
		e.apply(a.ID, decline(2, "2222"))
		if r := e.apply(a.ID, paidWith(500, "3333", 20)); r.Payment.Status != "succeeded" || r.Payment.HasFlag("late_success") {
			t.Fatalf("two declined cards: %s %v (a success inside the window is not late)", r.Payment.Status, r.Payment.Flags)
		}

		// The paying card declined before counts once: still two other cards.
		p = e.payment(500)
		a = e.attempt(p.ID)
		e.apply(a.ID, decline(1, "1111"))
		e.apply(a.ID, decline(2, "2222"))
		e.apply(a.ID, decline(3, "3333"))
		if r := e.apply(a.ID, paidWith(500, "3333", 20)); r.Payment.Status != "succeeded" {
			t.Fatalf("paying card declined first: %s", r.Payment.Status)
		}

		// Three other cards: review, with the money recorded and no payment.succeeded.
		p = e.payment(500)
		a = e.attempt(p.ID)
		for i, card := range []string{"1111", "2222", "3333"} {
			e.apply(a.ID, decline(i+1, card))
		}
		r := e.apply(a.ID, paidWith(500, "4444", 33))
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagDeclinedCards) || r.Payment.AmountReceived != 500 {
			t.Fatalf("three declined cards: %+v", r.Payment)
		}
		if ev := e.lastEvent(); ev.Type != "payment.needs_review" {
			t.Fatalf("last event %s", ev.Type)
		}
		if !slices.Contains(e.alertKinds(), "payment.needs_review") {
			t.Errorf("alerts %v", e.alertKinds())
		}

		// An operator approves it: succeeded, with payment.succeeded.
		got, err := e.svc.ApproveReview(ctx, audit.System, p.ID)
		if err != nil || got.Status != "succeeded" {
			t.Fatalf("approve: %s %v", got.Status, err)
		}
		if ev := e.lastEvent(); ev.Type != "payment.succeeded" {
			t.Fatalf("after approve: last event %s", ev.Type)
		}
		if _, err := e.svc.ApproveReview(ctx, audit.System, p.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Fatalf("approved twice: %v", err)
		}
	})
}

func TestApproveRefusesOtherReviews(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		r := e.apply(e.attempt(p.ID).ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 499, Currency: "INR"})
		if r.Payment.Status != "needs_review" {
			t.Fatalf("setup: %s", r.Payment.Status)
		}
		if _, err := e.svc.ApproveReview(ctx, audit.System, p.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Fatalf("an amount mismatch was approved: %v", err)
		}
	})
}

func TestDeclinedCardsStopThePayment(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		for i, card := range []string{"1111", "2222", "3333", "4444"} {
			e.apply(a.ID, decline(i+1, card))
		}
		if got := e.get(p.ID); got.HasFlag(core.FlagCardTesting) {
			t.Fatalf("flagged at four cards: %v", got.Flags)
		}
		e.apply(a.ID, decline(5, "1111")) // a card already counted
		if got := e.get(p.ID); got.HasFlag(core.FlagCardTesting) {
			t.Fatalf("a repeated card counted twice: %v", got.Flags)
		}
		e.apply(a.ID, decline(6, "5555"))
		if got := e.get(p.ID); !got.HasFlag(core.FlagCardTesting) {
			t.Fatalf("not flagged at five cards: %v", got.Flags)
		}
		if !slices.Contains(e.alertKinds(), "payment.card_testing") {
			t.Errorf("alerts %v", e.alertKinds())
		}
		if _, err := e.svc.CreateAttempt(ctx, p.ID, e.pa); !errors.Is(err, core.ErrInvalidState) {
			t.Fatalf("a stopped payment took a new attempt: %v", err)
		}
		// The provider's checkout may still be open: a success there goes to review.
		r := e.apply(a.ID, paidWith(500, "6666", 20))
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagCardTesting) {
			t.Fatalf("success after the stop: %+v", r.Payment)
		}
	})
}

func TestCustomerBlockedAfterDeclinedCards(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "customer_decline_limit", json.RawMessage(`6`)); err != nil {
			t.Fatal(err)
		}
		first := e.customerPayment(500, "u-1")
		a := e.attempt(first.ID)
		for i, card := range []string{"1111", "2222", "3333"} {
			e.apply(a.ID, decline(i+1, card))
		}
		second := e.customerPayment(500, "u-1")
		b := e.attempt(second.ID)
		e.apply(b.ID, decline(4, "1111")) // the same card again
		e.apply(b.ID, decline(5, "4444"))
		e.apply(b.ID, decline(6, "5555"))
		if _, err := e.svc.CreateAttempt(ctx, e.customerPayment(500, "u-1").ID, e.pa); err != nil {
			t.Fatalf("blocked at five cards: %v", err)
		}
		e.apply(b.ID, decline(7, "6666"))
		if !slices.Contains(e.alertKinds(), "customer.blocked") {
			t.Fatalf("no block at six cards: %v", e.alertKinds())
		}
		third := e.customerPayment(500, "u-1")
		if _, err := e.svc.CreateAttempt(ctx, third.ID, e.pa); !errors.Is(err, core.ErrInvalidState) {
			t.Fatalf("a blocked customer started an attempt: %v", err)
		}
		if _, err := e.svc.CreateAttempt(ctx, e.customerPayment(500, "u-2").ID, e.pa); err != nil {
			t.Fatalf("another customer was refused: %v", err)
		}

		// A success of theirs on an attempt already open goes to review.
		r := e.apply(b.ID, paidWith(500, "7777", 20))
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag(core.FlagCustomerBlocked) {
			t.Fatalf("blocked customer's success: %+v", r.Payment)
		}

		changed, err := e.svc.SetCustomerBlocked(ctx, audit.System, third.CustomerID, false)
		if err != nil || !changed {
			t.Fatalf("unblock: %v %v", changed, err)
		}
		if _, err := e.svc.CreateAttempt(ctx, third.ID, e.pa); err != nil {
			t.Fatalf("after unblock: %v", err)
		}
	})
}

func TestLaterTryKeepsThePayingTrysRisk(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		high := 92
		d := decline(1, "1111")
		d.Risk = connector.PaymentRisk{Score: &high}
		e.apply(a.ID, d)
		e.apply(a.ID, paidWith(500, "2222", 33))
		later := decline(2, "3333")
		later.Risk = connector.PaymentRisk{Score: &high}
		r := e.apply(a.ID, later)
		var risk connector.PaymentRisk
		if err := json.Unmarshal([]byte(r.Attempt.RiskDetails), &risk); err != nil || risk.Score == nil || *risk.Score != 33 {
			t.Fatalf("attempt risk %s, want the paying try's score 33", r.Attempt.RiskDetails)
		}
	})
}
