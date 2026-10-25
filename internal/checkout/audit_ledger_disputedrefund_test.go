package checkout_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
)

// Audit 2026-10 (ledger lane). F9 refuses a refund of ours on a disputed
// payment when it is created, but a refund created a moment before the dispute
// arrives is still pending, and refund.submit does not look again: the card
// network takes the money back and our refund pays the customer a second time.
func TestAuditLedgerPendingRefundIsNotSentOnceThePaymentIsDisputed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paid(t, e)
		r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 10000})
		if err != nil {
			t.Fatal(err)
		}
		// The dispute is opened (as disputes.upsert does: flag on the payment).
		if err := e.db.Write(ctx, func(tx store.Tx) error {
			cur, err := store.GetPaymentForUpdate(ctx, tx, p.ID)
			if err != nil {
				return err
			}
			cur.Flags = append(cur.Flags, "disputed")
			return store.UpdatePayment(ctx, tx, &cur, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
		_ = e.flow.SubmitRefund(ctx, submitPayload(r.ID))
		e.fake.Locked(func() {
			if n := len(e.fake.RefundCalls); n != 0 {
				t.Errorf("refund sent to the provider on a disputed payment (%d calls)", n)
			}
		})
	})
}
