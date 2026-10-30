package core_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
)

// R-A5 / H-1. Concurrent requests with one Idempotency-Key make one refund and
// all answer with it; the key cannot be reused for another refund.
func TestRefundIdempotencyKeyMakesOneRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paidPayment(t, e, 1000)
		in := core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 300, IdempotencyKey: "k-1"}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ids := map[string]bool{}
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := e.svc.CreateRefund(ctx, in)
				if err != nil {
					t.Errorf("refund: %v", err)
					return
				}
				mu.Lock()
				ids[r.ID] = true
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(ids) != 1 {
			t.Fatalf("one key made %d refunds", len(ids))
		}
		in.Amount = 400
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrConflict) {
			t.Errorf("the key reused for another amount: %v", err)
		}
		in.Amount = 300
		in.IdempotencyKey = ""
		if _, err := e.svc.CreateRefund(ctx, in); err != nil {
			t.Errorf("keyless refund: %v", err)
		}
	})
}
