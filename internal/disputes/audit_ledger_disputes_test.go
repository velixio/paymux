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
	// Open finding A-13 in docs/security-audit.md: refusing the second dispute
	// would drop a real chargeback, and capping its hold needs the release to
	// match. Remove the skip with the fix.
	t.Skip("A-13: unfixed, see docs/security-audit.md")
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		e.apply(e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{}))
		_, err := e.d.Apply(ctx, e.acct, e.snap("dsp_b", "pay_1", connector.DisputeNeedsResponse, time.Time{}), "webhook")
		if held := -sum(e.ledger(p.ID)); held > p.AmountReceived {
			t.Errorf("held %d of a payment that received %d (second apply err=%v)", held, p.AmountReceived, err)
		}
	})
}
