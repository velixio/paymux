package payouts_test

import (
	"errors"
	"testing"

	"github.com/paymux/paymux/internal/payouts"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

func payoutNote(snap connector.PayoutSnapshot) connector.Notification {
	return connector.Notification{Kind: connector.NotifyPayout, EventID: "evt_1", EventType: "payout.test", Payout: &snap}
}

// A payout webhook is a hint: the payout is fetched from the provider and that
// is what is applied, never the webhook's own status.
func TestPayoutNotificationIsFetchedThenApplied(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		p, _ := e.create(nil)
		e.run() // submitted: processing
		if got := e.get(p.ID); got.Status != "processing" {
			t.Fatalf("setup: %+v", got)
		}
		// The provider has paid it out; the webhook (forged or not) claims it failed.
		e.fake.Locked(func() {
			s := e.fake.Payouts[p.ProviderRef]
			s.Status, s.RawStatus = connector.PayoutSucceeded, "processed"
			e.fake.Payouts[p.ProviderRef] = s
		})
		lie := connector.PayoutSnapshot{ProviderRef: p.ProviderRef, Status: connector.PayoutFailed, Amount: p.Amount, Currency: p.Currency}
		if err := e.svc.SyncNotification(ctx, e.acct, payoutNote(lie)); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status != "succeeded" || got.CompletedAt.IsZero() {
			t.Errorf("payout = %+v, want the provider's truth (succeeded)", got)
		}
		// A second delivery changes nothing and emits nothing more.
		before := len(e.types())
		if err := e.svc.SyncNotification(ctx, e.acct, payoutNote(lie)); err != nil {
			t.Fatal(err)
		}
		if len(e.types()) != before {
			t.Errorf("a repeat delivery emitted events: %v", e.types())
		}
	})
}

func TestPayoutNotificationFoundByProviderPayoutID(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		p, _ := e.create(nil)
		e.run()
		got := e.get(p.ID)
		e.fake.Locked(func() {
			s := e.fake.Payouts[p.ProviderRef]
			s.Status = connector.PayoutReversed
			e.fake.Payouts[p.ProviderRef] = s
		})
		// Only the provider's own id on the webhook, not our reference.
		if err := e.svc.SyncNotification(ctx, e.acct, payoutNote(connector.PayoutSnapshot{ProviderPayoutID: got.ProviderPayoutID})); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status != "reversed" {
			t.Errorf("payout = %+v", got)
		}
	})
}

func TestPayoutNotificationForAnUnknownOrForeignPayoutIsNotApplied(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		p, _ := e.create(nil)
		e.run()
		note := payoutNote(connector.PayoutSnapshot{ProviderRef: "po_nobody", Status: connector.PayoutSucceeded})
		if err := e.svc.SyncNotification(ctx, e.acct, note); !errors.Is(err, payouts.ErrUnknownPayout) {
			t.Errorf("unknown payout: %v", err)
		}
		// Our reference, delivered on another provider account: not that account's payout.
		mine := payoutNote(connector.PayoutSnapshot{ProviderRef: p.ProviderRef, Status: connector.PayoutSucceeded})
		if err := e.svc.SyncNotification(ctx, "pa_another_account", mine); !errors.Is(err, payouts.ErrUnknownPayout) {
			t.Errorf("another account: %v", err)
		}
		if got := e.get(p.ID); got.Status != "processing" {
			t.Errorf("payout = %+v", got)
		}
	})
}

// The provider's answer must be about our payout: a payout of another amount
// or reference is not booked on it.
func TestPayoutFetchedForADifferentPayoutIsNotApplied(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, []string{"payout"})
		p, _ := e.create(nil)
		e.run()
		for name, mutate := range map[string]func(*connector.PayoutSnapshot){
			"other amount":    func(s *connector.PayoutSnapshot) { s.Amount = p.Amount + 1 },
			"other currency":  func(s *connector.PayoutSnapshot) { s.Currency = "USD" },
			"other reference": func(s *connector.PayoutSnapshot) { s.ProviderRef = "po_someone_else" },
		} {
			e.fake.Locked(func() {
				s := e.fake.Payouts[p.ProviderRef]
				s.Status = connector.PayoutSucceeded
				mutate(&s)
				e.fake.Payouts[p.ProviderRef] = s
			})
			_ = e.svc.SyncNotification(ctx, e.acct, payoutNote(connector.PayoutSnapshot{ProviderRef: p.ProviderRef}))
			if got := e.get(p.ID); got.Status != "processing" {
				t.Errorf("%s: payout moved to %s", name, got.Status)
			}
			e.fake.Locked(func() {
				e.fake.Payouts[p.ProviderRef] = connector.PayoutSnapshot{ProviderRef: p.ProviderRef, ProviderPayoutID: p.ProviderPayoutID,
					Status: connector.PayoutProcessing, Amount: p.Amount, Currency: p.Currency}
			})
		}
	})
}
