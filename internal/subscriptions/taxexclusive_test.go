package subscriptions_test

import (
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
)

// With prices that exclude GST the plan price is the taxable value and the
// customer pays taxable + GST: the registration, the pre-debit notice and every
// debit say so, and the payment settles at that amount (no needs_review).
func TestTaxExclusiveRenewalsChargeTaxablePlusGST(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		if err := db.Write(ctx, func(tx store.Tx) error {
			return store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: true,
				SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: false}, t0)
		}); err != nil {
			t.Fatal(err)
		}
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Amount = 100000 }) // Rs 1000 plus GST
		if _, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c)); err != nil {
			t.Fatal(err)
		}
		sub := e.activateMandate(c)
		if got := e.r.mandateReqs[len(e.r.mandateReqs)-1].Amount; got != 118000 {
			t.Errorf("the registration asks the provider to debit %d, want 118000 (1000 + 18%% GST)", got)
		}
		e.runJobs()
		sub = e.sub(sub.ID)
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Status != "succeeded" || pays[0].Amount != 118000 || pays[0].AmountReceived != 118000 {
			t.Fatalf("period 0: %+v", pays)
		}
		if sub.RenewalsDone != 1 {
			t.Fatalf("renewals done %d", sub.RenewalsDone)
		}
		// The next period: notice and debit are both 1180.
		e.advanceTo(sub.CurrentPeriodEnd.Add(-48 * 3600e9))
		if len(e.r.notices) != 1 || e.r.notices[0].Amount != 118000 {
			t.Fatalf("notices = %+v", e.r.notices)
		}
		e.advanceTo(sub.CurrentPeriodEnd)
		if e.r.chargeCount() != 1 || e.r.charges[0].Amount != 118000 {
			t.Fatalf("charges = %+v", e.r.charges)
		}
		pays = e.renewalPayments(sub.ID)
		if len(pays) != 2 || pays[1].Status != "succeeded" || pays[1].Amount != 118000 || pays[1].AmountReceived != 118000 || pays[1].HasFlag("amount_mismatch") {
			t.Fatalf("renewal payment: %+v", pays)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			invs, _ := store.InvoicesForPayment(ctx, q, pays[1].ID)
			if len(invs) != 1 || invs[0].Total != 118000 || invs[0].Taxable != 100000 {
				t.Errorf("renewal invoice = %+v", invs)
			}
			return nil
		})
	})
}

// A debit that fails and is retried keeps asking for the same amount.
func TestTaxExclusiveDunningRetryChargesTheSameAmount(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		if err := db.Write(ctx, func(tx store.Tx) error {
			return store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: true,
				SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: false}, t0)
		}); err != nil {
			t.Fatal(err)
		}
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Amount = 100000 })
		if _, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c)); err != nil {
			t.Fatal(err)
		}
		sub := e.activateMandate(c)
		e.runJobs()
		sub = e.sub(sub.ID)
		e.r.mu.Lock()
		e.r.chargeFn = failing("insufficient_funds")
		e.r.mu.Unlock()
		e.advanceTo(sub.CurrentPeriodEnd.Add(-48 * 3600e9))
		e.advanceTo(sub.CurrentPeriodEnd)
		e.r.mu.Lock()
		first := len(e.r.charges)
		e.r.chargeFn = nil
		e.r.mu.Unlock()
		if first < 1 {
			t.Fatal("no charge was attempted")
		}
		e.advanceTo(sub.CurrentPeriodEnd.AddDate(0, 0, 3))
		e.r.mu.Lock()
		defer e.r.mu.Unlock()
		for i, ch := range e.r.charges {
			if ch.Amount != 118000 {
				t.Errorf("charge %d asked for %d, want 118000", i, ch.Amount)
			}
		}
		for i, n := range e.r.notices {
			if n.Amount != 118000 {
				t.Errorf("notice %d says %d, want 118000", i, n.Amount)
			}
		}
	})
}
