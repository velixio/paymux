package paylinks_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// su is an env whose core runs the link's hook, as the hub wires it, and which
// can pay a payment through an attempt.
func su(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := newEnv(t, db)
	if err := db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "test",
			Connector: "fake", Name: "Fake", Status: "active", CreatedAt: e.now})
	}); err != nil {
		t.Fatal(err)
	}
	var links *paylinks.Service
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Emitter: e.emit, Now: e.clock,
		OnPaymentSucceeded: func(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt) error {
			return links.OnPaymentSucceeded(ctx, tx, p, a)
		}})
	e.links = paylinks.New(paylinks.Options{DB: db, Core: e.core, Emitter: e.emit, BaseURL: baseURL, Now: e.clock})
	links = e.links
	return e
}

func (e *env) pay(paymentID string) store.Attempt {
	e.t.Helper()
	a, err := e.core.CreateAttempt(ctx, paymentID, "pa_1")
	if err != nil {
		e.t.Fatal(err)
	}
	p, _ := e.core.GetPaymentByID(ctx, paymentID)
	if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: p.Amount, Currency: p.Currency}); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) payment(id string) store.Payment {
	e.t.Helper()
	p, err := e.core.GetPaymentByID(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) paymentsOfLink(linkID string) (n int) {
	e.t.Helper()
	_ = e.db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM payments WHERE order_ref = ?", linkID).Scan(&n)
	})
	return n
}

// The visitor who holds a single-use link and submits the form again (back
// button, double click) gets the payment they already have, not a second one.
func TestSameVisitorGetsTheirOpenPaymentBack(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := su(t, db)
		l := e.create(func(in *paylinks.CreateInput) { in.SingleUse = true })
		who := paylinks.StartInput{Name: "A", Email: "a@example.com", Holder: "visitor-a"}
		first, err := e.links.Start(ctx, l.Slug, who)
		if err != nil {
			t.Fatal(err)
		}
		second, err := e.links.Start(ctx, l.Slug, who)
		if err != nil {
			t.Fatal(err)
		}
		if second.PaymentID != first.PaymentID || e.paymentsOfLink(l.ID) != 1 {
			t.Fatalf("second start made another payment: %s vs %s (%d payments)", second.PaymentID, first.PaymentID, e.paymentsOfLink(l.ID))
		}
		p := e.payment(second.PaymentID)
		if !core.ClientSecretMatches(p, second.ClientSecret) {
			t.Error("the secret handed back does not open the payment")
		}
		if core.ClientSecretMatches(p, first.ClientSecret) {
			t.Error("the earlier secret should have been replaced")
		}
		// Once that payment is over (canceled), the visitor may start a new one.
		if _, err := e.core.CancelPayment(ctx, e.scope, audit.System, first.PaymentID); err != nil {
			t.Fatal(err)
		}
		third, err := e.links.Start(ctx, l.Slug, who)
		if err != nil || third.PaymentID == first.PaymentID {
			t.Fatalf("after cancel: %+v %v", third, err)
		}
	})
}

// When one payment spends the link, its other open payments are canceled.
func TestSpendingTheLinkCancelsItsOtherOpenPayments(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := su(t, db)
		l := e.create(func(in *paylinks.CreateInput) { in.SingleUse = true })
		a, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: "A", Email: "a@example.com", Holder: "visitor-a"})
		if err != nil {
			t.Fatal(err)
		}
		e.advance(61 * time.Minute) // the hold lapses
		b, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: "B", Email: "b@example.com", Holder: "visitor-b"})
		if err != nil {
			t.Fatal(err)
		}
		e.pay(a.PaymentID)
		if got := e.payment(b.PaymentID); got.Status != "canceled" {
			t.Errorf("the other open payment is %s, want canceled", got.Status)
		}
		if got := e.payment(a.PaymentID); got.Status != "succeeded" || got.HasFlag("link_already_used") {
			t.Errorf("the paying one: %s %v", got.Status, got.Flags)
		}
	})
}

// A payment that succeeds on a link somebody else already spent is not silently
// accepted: it is flagged for review with an alert, or refunded automatically
// when duplicate_payment_action says so.
func TestSuccessOnASpentLinkIsFlaggedOrRefunded(t *testing.T) {
	for _, action := range []string{"review", "auto_refund"} {
		t.Run(action, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				e := su(t, db)
				if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "duplicate_payment_action", json.RawMessage(`"`+action+`"`)); err != nil {
					t.Fatal(err)
				}
				l := e.create(func(in *paylinks.CreateInput) { in.SingleUse = true })
				a, err := e.links.Start(ctx, l.Slug, paylinks.StartInput{Name: "A", Email: "a@example.com", Holder: "visitor-a"})
				if err != nil {
					t.Fatal(err)
				}
				// A second payment of the same link exists (started before the spend, and
				// beyond the reach of the cancellation: it was created without the hold).
				meta := map[string]string{paylinks.MetadataKey: l.ID}
				res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: l.Amount, Currency: l.Currency, OrderRef: l.ID + "-late",
					Metadata: meta, Customer: &core.CustomerInput{Name: "B", Email: "b@example.com"}})
				if err != nil {
					t.Fatal(err)
				}
				e.pay(a.PaymentID)
				e.pay(res.Payment.ID)
				got := e.payment(res.Payment.ID)
				if got.Status != "succeeded" && got.Status != "canceled" && got.Status != "needs_review" {
					t.Fatalf("status %s", got.Status)
				}
				if !got.HasFlag("link_already_used") && !got.HasFlag("success_after_cancel") {
					t.Fatalf("the second sale of a single-use link went through unflagged: %s %v", got.Status, got.Flags)
				}
				if action == "auto_refund" && got.HasFlag("link_already_used") {
					var n int
					_ = db.Read(ctx, func(q store.Querier) error {
						return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM refunds WHERE payment_id = ? AND reason = 'payment_link_already_used'", got.ID).Scan(&n)
					})
					if n != 1 {
						t.Errorf("refunds for the second sale: %d", n)
					}
				}
			})
		})
	}
}
