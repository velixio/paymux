package disputes_test

import (
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/pkg/connector"
)

// Audit 2026-10 (ledger lane). Two disputes are open on one payment; one is
// won. The other still holds the money, so a refund of ours would still pay the
// customer twice (F9). The "disputed" flag is a single bit dropped by the first
// dispute to resolve.
func TestAuditLedgerSecondOpenDisputeStillBlocksRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		a := e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		a.Amount = 20000
		b := e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		b.Amount = 20000
		e.apply(a)
		e.apply(b)
		a.Status = connector.DisputeWon
		e.apply(a)
		if got := e.payment(p.ID); !got.HasFlag("disputed") {
			t.Errorf("dispute dsp_b is still open but the payment lost its disputed flag: %v", got.Flags)
		}
		_, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api",
			PaymentID: p.ID, Amount: 50000})
		if !errors.Is(err, core.ErrNotRefundable) {
			t.Errorf("full refund accepted while dispute dsp_b holds 20000: err=%v", err)
		}
	})
}

// Audit 2026-10 (ledger lane). F11 checks each dispute against the amount
// received, but not the sum of the open holds: two full-amount disputes hold
// twice what was received.
func TestAuditLedgerDisputeHoldsCannotExceedReceived(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		e.apply(e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{}))
		_, err := e.d.Apply(ctx, e.acct, e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{}), "webhook")
		if held := -sum(e.ledger(p.ID)); held > p.AmountReceived {
			t.Errorf("held %d of a payment that received %d (second apply err=%v)", held, p.AmountReceived, err)
		}
	})
}

// A-13. Two full-amount disputes on one payment, one lost and one won, in
// either order: the dispute entries never take more than the payment received
// out of the ledger, and a dispute that ends releases exactly what it held.
func TestDisputeHoldsOfTwoDisputesNetToAtMostTheAmountReceived(t *testing.T) {
	for _, lostFirst := range []bool{true, false} {
		each(t, func(t *testing.T, e *env) {
			p, _ := e.paid("pay_1")
			a := e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{})
			b := e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{})
			e.apply(a)
			e.apply(b)
			if held := -sum(e.ledger(p.ID)); held > p.AmountReceived {
				t.Errorf("lostFirst=%v: both open, %d held of %d received", lostFirst, held, p.AmountReceived)
			}
			first, second := connector.DisputeLost, connector.DisputeWon
			if !lostFirst {
				first, second = second, first
			}
			a.Status, b.Status = first, second
			e.apply(a)
			e.apply(b)
			got := sum(e.ledger(p.ID))
			if got < -p.AmountReceived {
				t.Errorf("lostFirst=%v: disputes took %d from a payment that received %d", lostFirst, -got, p.AmountReceived)
			}
			if lostFirst && got != -p.AmountReceived {
				t.Errorf("the lost dispute should cost the amount it held: dispute entries sum to %d, want %d", got, -p.AmountReceived)
			}
		})
	}
}
