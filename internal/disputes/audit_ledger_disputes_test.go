package disputes_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/disputes"
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
// out of the ledger, and the lost one takes all of it, even when it was capped
// at the open and the other dispute was won first.
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
			// Whichever order they end in, the lost dispute takes the amount received:
			// when the other one ended first, its share was freed and the hold is topped up.
			if got != -p.AmountReceived {
				t.Errorf("lostFirst=%v: dispute entries sum to %d, want %d", lostFirst, got, -p.AmountReceived)
			}
		})
	}
}

// A-13. An overlapping dispute is stored with the provider's amount and holds
// only what is left; the payment is flagged and ops are told. The hold is
// released and lost at the amount held.
func TestOverlappingDisputeKeepsTheProvidersAmountAndHoldsTheRest(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		a := e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		a.Amount = 30000
		b := e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{}) // 50000
		ra, rb := e.apply(a), e.apply(b)
		if ra.Dispute.AmountHeld != 30000 || ra.Dispute.Amount != 30000 {
			t.Errorf("first dispute: %+v", ra.Dispute)
		}
		if rb.Dispute.Amount != 50000 || rb.Dispute.AmountHeld != 20000 {
			t.Errorf("second dispute: amount %d held %d, want 50000 and 20000", rb.Dispute.Amount, rb.Dispute.AmountHeld)
		}
		if !e.payment(p.ID).HasFlag(disputes.FlagDisputeOverlap) {
			t.Errorf("no %s flag: %v", disputes.FlagDisputeOverlap, e.payment(p.ID).Flags)
		}
		if k := e.alerts.kinds(); !slices.Contains(k, "dispute.overlap") {
			t.Errorf("alerts %v", k)
		}
		if got := sum(e.ledger(p.ID)); got != -50000 {
			t.Errorf("held %d, want 50000", -got)
		}
		b.Status = connector.DisputeLost
		e.apply(b)
		if got := sum(e.ledger(p.ID)); got != -50000 { // a still holds 30000, b lost 20000
			t.Errorf("after losing b: %d, want -50000", got)
		}
		a.Status = connector.DisputeWon
		e.apply(a)
		if got := sum(e.ledger(p.ID)); got != -20000 {
			t.Errorf("after winning a: %d, want -20000", got)
		}
	})
}

// A hold that is capped to nothing moves no money, at the hold or at the end.
func TestFullyCappedDisputeBooksNothing(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		e.apply(e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{}))
		b := e.apply(e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{}))
		if b.Dispute.AmountHeld != 0 || len(e.ledger(p.ID)) != 1 {
			t.Errorf("held %d with %d dispute entries", b.Dispute.AmountHeld, len(e.ledger(p.ID)))
		}
		s := e.snap("dsp_b", "pay_1", connector.DisputeWon, time.Time{})
		e.apply(s)
		if len(e.ledger(p.ID)) != 1 {
			t.Errorf("a capped dispute ended with %d dispute entries", len(e.ledger(p.ID)))
		}
	})
}

// A-13. Two full-amount disputes that are both lost take the amount received
// once, not twice.
func TestTwoLostDisputesNeverTakeMoreThanReceived(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		a := e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		b := e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		e.apply(a)
		e.apply(b)
		a.Status, b.Status = connector.DisputeLost, connector.DisputeLost
		e.apply(a)
		e.apply(b)
		if got := sum(e.ledger(p.ID)); got != -p.AmountReceived {
			t.Errorf("dispute entries sum to %d, want %d", got, -p.AmountReceived)
		}
		if !e.payment(p.ID).HasFlag(disputes.FlagDisputeOverlap) {
			t.Errorf("no overlap flag: %v", e.payment(p.ID).Flags)
		}
	})
}
