package disputes_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

func (e *env) refundStatus(id string) (st string) {
	e.t.Helper()
	_ = e.db.Read(ctx, func(q store.Querier) error {
		r, err := store.GetRefund(ctx, q, id)
		st = r.Status
		return err
	})
	return
}

func (e *env) refundJob(refundID string) (state string, attempts int) {
	e.t.Helper()
	if err := e.db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT state, attempts FROM jobs WHERE dedupe_key = ?", "refund.submit:"+refundID).Scan(&state, &attempts)
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) newRefund(paymentID string, amount int64) store.Refund {
	e.t.Helper()
	r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: paymentID, Amount: amount})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) setJob(refundID, state string, attempts int) {
	e.t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE jobs SET state = ?, attempts = ? WHERE dedupe_key = ?", state, attempts, "refund.submit:"+refundID)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) countEmitted(typ string) (n int) {
	for _, ty := range e.emitted() {
		if ty == typ {
			n++
		}
	}
	return
}

// R-A7. A refund of ours waits for its submit job; a dispute opens meanwhile.
// A refund whose job has never been claimed is failed in the dispute's own
// transaction and its job cancelled, so nothing is ever sent.
func TestDisputeOpeningFailsARefundWhoseSubmitJobNeverRan(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		r := e.newRefund(p.ID, 10000)
		if st, n := e.refundJob(r.ID); st != "pending" || n != 0 {
			t.Fatalf("setup: job %s/%d", st, n)
		}
		e.apply(e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{}))
		if got := e.refundStatus(r.ID); got != "failed" {
			t.Errorf("the unsent refund is %s, want failed", got)
		}
		if st, n := e.refundJob(r.ID); st == "pending" || st == "running" || n != 0 {
			t.Errorf("the refund job is %s after %d attempts: it must not run", st, n)
		}
		if n := e.countEmitted("refund.failed"); n != 1 {
			t.Errorf("refund.failed sent %d times: %v", n, e.emitted())
		}
	})
}

// A refund whose job has been tried (an ambiguous error, a lost answer) or is
// running may already be with the provider: it is left as it is. Failing it
// would be final, and a later provider success could not move it.
func TestDisputeOpeningLeavesARefundThatMayBeWithTheProvider(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		retried := e.newRefund(p.ID, 10000)
		running := e.newRefund(p.ID, 5000)
		done := e.newRefund(p.ID, 2000)
		e.setJob(retried.ID, "pending", 1) // tried once, waiting to retry
		e.setJob(running.ID, "running", 1)
		e.setJob(done.ID, "done", 1)

		e.apply(e.snap("dsp_a", "pay_1", connector.DisputeNeedsResponse, time.Time{}))

		for name, r := range map[string]store.Refund{"retried": retried, "running": running, "done": done} {
			if got := e.refundStatus(r.ID); got != "pending" {
				t.Errorf("%s refund is %s, want pending", name, got)
			}
		}
		if st, n := e.refundJob(retried.ID); st != "pending" || n != 1 {
			t.Errorf("the retried job was touched: %s/%d", st, n)
		}
		if n := e.countEmitted("refund.failed"); n != 0 {
			t.Errorf("refund.failed sent %d times", n)
		}
	})
}
