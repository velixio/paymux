package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestOnPaymentSucceededHook(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		var calls []store.Attempt
		var fail error
		e.svc = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: e.q,
			OnPaymentSucceeded: func(_ context.Context, tx store.Tx, p store.Payment, a store.Attempt) error {
				if p.Status != "succeeded" || p.AmountReceived != 5000 {
					t.Errorf("hook saw payment %+v", p)
				}
				calls = append(calls, a)
				return fail
			}})
		p := e.payment(5000)
		a1 := e.attempt(p.ID)
		a2 := e.attempt(p.ID)

		// A hook error rolls the whole transition back.
		fail = errors.New("boom")
		if _, err := e.svc.ApplyAttemptSnapshot(ctx, a1.ID, captured(5000)); err == nil {
			t.Fatal("hook error swallowed")
		}
		got, _ := e.svc.GetAttempt(ctx, a1.ID)
		if got.Status == "captured" {
			t.Error("attempt moved despite the hook failing")
		}
		fail = nil
		calls = nil

		e.apply(a1.ID, captured(5000))
		if len(calls) != 1 || calls[0].ID != a1.ID || calls[0].CurrencyReceived != "INR" || calls[0].AmountReceived != 5000 {
			t.Fatalf("calls = %+v", calls)
		}
		// A repeated snapshot and a duplicate success do not call the hook again.
		e.apply(a1.ID, captured(5000))
		e.apply(a2.ID, captured(5000))
		if len(calls) != 1 {
			t.Fatalf("hook ran for repeat or duplicate: %d calls", len(calls))
		}
	})
}

func TestRefundSucceededHookRunsOnlyWhenSet(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(5000)
		a := e.attempt(p.ID)
		e.apply(a.ID, captured(5000))
		r, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 1000})
		if err != nil {
			t.Fatal(err)
		}
		// No hook: no-op.
		if err := db.Write(ctx, func(tx store.Tx) error { return e.svc.RefundSucceeded(ctx, tx, r) }); err != nil {
			t.Fatal(err)
		}
		var seen store.Refund
		var seenAttempt string
		svc := core.NewService(core.Options{DB: db, Settings: e.set, Jobs: e.q,
			OnRefundSucceeded: func(_ context.Context, _ store.Tx, r store.Refund, p store.Payment, a store.Attempt) error {
				seen, seenAttempt = r, a.ID
				if p.ID != r.PaymentID {
					t.Errorf("payment %s for refund of %s", p.ID, r.PaymentID)
				}
				return nil
			}})
		if err := db.Write(ctx, func(tx store.Tx) error { return svc.RefundSucceeded(ctx, tx, r) }); err != nil {
			t.Fatal(err)
		}
		if seen.ID != r.ID || seenAttempt != a.ID {
			t.Errorf("seen %+v attempt %s", seen, seenAttempt)
		}
	})
}
