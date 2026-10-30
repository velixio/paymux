package core_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
)

// H-13. Every charge and refund entry names its attempt, so the extra charge
// of a double payment, and its refund, can be told apart from the sale's. The
// revenue totals count the sale once.
func TestLedgerTotalsCountTheSaleNotTheDuplicateCharge(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		first, second := captured(500), captured(500)
		first.ProviderPaymentID, second.ProviderPaymentID = "pp_sale", "pp_dup"
		e.apply(a1.ID, first)
		e.apply(a2.ID, second)

		if n := e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'charge' AND attempt_id = ?", a1.ID); n != 1 {
			t.Errorf("%d charge entries name the sale's attempt", n)
		}
		if n := e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'charge' AND attempt_id = ?", a2.ID); n != 1 {
			t.Errorf("%d charge entries name the duplicate attempt", n)
		}
		totals := func() store.LedgerTotal {
			var out []store.LedgerTotal
			_ = e.db.Read(ctx, func(q store.Querier) (err error) {
				out, err = store.LedgerTotals(ctx, q, store.DefaultOrgID, "test", time.Time{})
				return
			})
			if len(out) != 1 {
				t.Fatalf("totals %+v", out)
			}
			return out[0]
		}
		if got := totals(); got.Gross != 500 || got.Refunds != 0 {
			t.Errorf("a double payment counted as revenue: %+v", got)
		}

		// The duplicate is refunded: still not part of the sale's totals.
		rf, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "dashboard", PaymentID: p.ID, AttemptID: a2.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ApplyRefundSnapshot(ctx, rf.ID, core.RefundSnapshot{Status: state.RefundSucceeded}); err != nil {
			t.Fatal(err)
		}
		if n := e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'refund' AND attempt_id = ?", a2.ID); n != 1 {
			t.Errorf("%d refund entries name the duplicate attempt", n)
		}
		if got := totals(); got.Gross != 500 || got.Refunds != 0 {
			t.Errorf("the refund of a duplicate counted against the sale: %+v", got)
		}

		// A real refund of the sale is counted.
		real, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "dashboard", PaymentID: p.ID, Amount: 200})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.ApplyRefundSnapshot(ctx, real.ID, core.RefundSnapshot{Status: state.RefundSucceeded}); err != nil {
			t.Fatal(err)
		}
		if got := totals(); got.Gross != 500 || got.Refunds != 200 {
			t.Errorf("after a refund of the sale: %+v", got)
		}
	})
}
