package core_test

import (
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
)

// R-A3. Two concurrent returns can each pass the read check that a provider
// payment is not another attempt's. The database settles it: of two payments'
// attempts applied with the same provider payment, exactly one is captured; the
// other changes nothing and is not an error (it is "not ours", as when the
// read check rejects it).
func TestOneProviderPaymentSettlesOneAttempt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p1, p2 := e.payment(1000), e.payment(1000)
		a1, a2 := e.attempt(p1.ID), e.attempt(p2.ID)
		snap := core.Snapshot{Status: state.AttemptCaptured, Amount: 1000, Currency: "INR", ProviderPaymentID: "pay_shared"}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, id := range []string{a1.ID, a2.ID} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := e.svc.ApplyAttemptSnapshot(ctx, id, snap); err != nil {
					t.Errorf("apply to %s: %v", id, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		captured := 0
		for _, id := range []string{a1.ID, a2.ID} {
			var a store.Attempt
			_ = e.db.Read(ctx, func(q store.Querier) (err error) { a, err = store.GetAttempt(ctx, q, id); return })
			if a.Status == string(state.AttemptCaptured) {
				captured++
			}
		}
		if captured != 1 {
			t.Errorf("%d attempts captured by one provider payment", captured)
		}
		paid := 0
		for _, id := range []string{p1.ID, p2.ID} {
			if e.get(id).Status == string(state.PaymentSucceeded) {
				paid++
			}
		}
		if paid != 1 {
			t.Errorf("%d payments succeeded on one provider payment", paid)
		}
		if n := e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'charge'"); n != 1 {
			t.Errorf("%d charge entries for one provider payment", n)
		}
	})
}

// Duplicate successes (F31) stay allowed: a second captured attempt of an
// already paid payment is recorded with the same provider payment id.
func TestDuplicateSuccessOfOnePaymentStillRecorded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(1000)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		snap := core.Snapshot{Status: state.AttemptCaptured, Amount: 1000, Currency: "INR", ProviderPaymentID: "pay_dup"}
		e.apply(a1.ID, snap)
		if r := e.apply(a2.ID, snap); !r.Changed || !r.Attempt.Duplicate {
			t.Errorf("second success of the payment: %+v", r.Attempt)
		}
	})
}
