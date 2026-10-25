package core_test

import (
	"sync"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
)

// Audit 2026-10 (ledger lane). Concurrent refunds of one payment never add up
// to more than was received, and applying the same success concurrently books
// one ledger entry.
func TestAuditLedgerConcurrentRefundsNeverOverRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paidPayment(t, e, 1000)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var made []store.Refund
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 300})
				if err == nil {
					mu.Lock()
					made = append(made, r)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(made) != 3 {
			t.Fatalf("%d refunds of 300 created on 1000", len(made))
		}
		for _, r := range made {
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundSucceeded, ProviderRefundID: "rf_" + r.ID})
				}()
			}
		}
		wg.Wait()
		var l []store.LedgerEntry
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { l, err = store.ListLedgerEntries(ctx, q, p.ID); return })
		var refunds int
		var total int64
		for _, x := range l {
			if x.Kind == "refund" {
				refunds++
				total += x.Amount
			}
		}
		if got := e.get(p.ID); refunds != 3 || total != -900 || got.AmountRefunded != 900 {
			t.Errorf("refund ledger entries %d total %d amount_refunded %d", refunds, total, got.AmountRefunded)
		}
	})
}
