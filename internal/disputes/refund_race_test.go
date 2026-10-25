package disputes_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// R-A7. A refund of ours waits for its submit job; a dispute opens meanwhile.
// The dispute fails the refunds that have not been sent, in its own
// transaction, so the job finds nothing to send. One already with the provider
// (its id is known) is left alone.
func TestDisputeOpeningFailsThePendingUnsentRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		mk := func(amount int64) store.Refund {
			r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: amount})
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		unsent, sent := mk(10000), mk(5000)
		if err := e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE refunds SET provider_refund_id = 're_1' WHERE id = ?", sent.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		e.apply(e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{}))

		status := func(id string) (st string) {
			_ = e.db.Read(ctx, func(q store.Querier) error {
				r, err := store.GetRefund(ctx, q, id)
				st = r.Status
				return err
			})
			return
		}
		if got := status(unsent.ID); got != "failed" {
			t.Errorf("the unsent refund is %s, want failed", got)
		}
		if got := status(sent.ID); got != "pending" {
			t.Errorf("a refund already with the provider is %s, want pending", got)
		}
		n := 0
		for _, ty := range e.emitted() {
			if ty == "refund.failed" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("refund.failed sent %d times: %v", n, e.emitted())
		}
		// The failure carries its reason.
		if c := e.count("SELECT COUNT(*) FROM refunds WHERE id = ? AND status = 'failed'", unsent.ID); c != 1 {
			t.Errorf("refund rows failed = %d", c)
		}
	})
}
