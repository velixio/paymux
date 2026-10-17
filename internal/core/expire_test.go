package core_test

import (
	"slices"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
)

// backdate moves a payment's expiry into the past.
func (e *env) backdate(id string, ago time.Duration) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE payments SET expires_at = ? WHERE id = ?", time.Now().Add(-ago).UnixMilli(), id)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestExpireDue(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		idle := e.payment(100) // expired, nothing ever happened
		e.backdate(idle.ID, time.Minute)

		abandoned := e.payment(100) // expired; an attempt was created but never reached a provider
		abandonedAttempt := e.attempt(abandoned.ID)
		e.backdate(abandoned.ID, time.Minute)

		inFlight := e.payment(100) // expired, but a customer is mid-payment at the provider
		a := e.attempt(inFlight.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending})
		e.backdate(inFlight.ID, time.Minute)

		started := e.payment(100) // expired; an attempt got a provider order and is still polled
		sa := e.attempt(started.ID)
		if err := e.svc.RecordCheckout(ctx, sa.ID, "order_1"); err != nil {
			t.Fatal(err)
		}
		e.backdate(started.ID, time.Minute)

		fresh := e.payment(100) // still valid
		paid := e.payment(100)  // succeeded
		e.apply(e.attempt(paid.ID).ID, captured(100))
		e.backdate(paid.ID, time.Minute)
		canceled := e.payment(100)
		if _, err := e.svc.CancelPayment(ctx, e.scope, audit.System, canceled.ID); err != nil {
			t.Fatal(err)
		}
		e.backdate(canceled.ID, time.Minute)

		before := len(e.emitted())
		n, err := e.svc.ExpireDue(ctx, 100)
		if err != nil || n != 2 {
			t.Fatalf("expired %d, err %v (want the idle and the abandoned payment)", n, err)
		}
		for id, want := range map[string]string{idle.ID: "expired", abandoned.ID: "expired", inFlight.ID: "requires_action",
			started.ID: "created", fresh.ID: "created", paid.ID: "succeeded", canceled.ID: "canceled"} {
			if got := e.get(id).Status; got != want {
				t.Errorf("%s: %s, want %s", id, got, want)
			}
		}
		ev := e.emitted()[before:]
		if !slices.Equal(ev, []string{"payment.expired", "payment.expired"}) {
			t.Errorf("events %v", ev)
		}
		if got := e.get(idle.ID); got.Version != idle.Version+1 {
			t.Errorf("version %d", got.Version)
		}
		if n, err := e.svc.ExpireDue(ctx, 100); err != nil || n != 0 {
			t.Errorf("second run expired %d: %v", n, err)
		}
		// The in-flight payment can still succeed after its link expired, and an
		// expired payment still accepts a late success.
		e.apply(a.ID, captured(100))
		if got := e.get(inFlight.ID); got.Status != "succeeded" {
			t.Errorf("in-flight payment: %s", got.Status)
		}
		e.apply(abandonedAttempt.ID, captured(100))
		if got := e.get(abandoned.ID); got.Status != "succeeded" || !got.HasFlag("late_success") {
			t.Errorf("late success after expiry: %+v", got)
		}
	})
}

func TestExpireDueRespectsLimitAndIgnoresStaleAttempts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		for i := 0; i < 3; i++ {
			p := e.payment(100)
			e.backdate(p.ID, time.Minute)
		}
		if n, _ := e.svc.ExpireDue(ctx, 2); n != 2 {
			t.Errorf("limit: %d", n)
		}
		if n, _ := e.svc.ExpireDue(ctx, 2); n != 1 {
			t.Errorf("rest: %d", n)
		}
		// A pending attempt older than the 72h polling window no longer holds the payment open.
		p := e.payment(100)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending})
		e.backdate(p.ID, time.Minute)
		err := e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE payment_attempts SET created_at = ? WHERE id = ?", time.Now().Add(-100*time.Hour).UnixMilli(), a.ID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := e.svc.ExpireDue(ctx, 10); n != 1 || e.get(p.ID).Status != "expired" {
			t.Errorf("stale attempt kept the payment open: %d %s", n, e.get(p.ID).Status)
		}
	})
}
