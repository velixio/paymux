package subscriptions_test

import (
	"errors"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/subscriptions"
)

// F-A: Razorpay takes USD for one-time payments but bills subscriptions in INR
// only. A USD subscription must not be routed to it (the customer would reach a
// checkout that fails with "Currency is not supported"): it falls through to the
// next provider, or is refused at creation with no_route.
func TestSubscriptionCurrencyIsCheckedAtCreationNotAtCheckout(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE provider_accounts SET accepted_currencies = '["INR","USD"]' WHERE id IN ('pa_managed','pa_whop')`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		usd := func(i *subscriptions.CreateInput) { i.Currency = "USD"; i.Method = "" }

		e.routeTo("razorpay", "whop")
		c := e.mustCreate(usd)
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_whop" || sub.BillingMode != "provider_managed" {
			t.Errorf("USD went to %s (%s), want the next target, whop", sub.ProviderAccountID, sub.BillingMode)
		}
		// In INR Razorpay is still first.
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "" })
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_managed" {
			t.Errorf("INR went to %s, want razorpay", sub.ProviderAccountID)
		}

		e.routeTo("razorpay")
		if _, err := e.create(usd); !errors.Is(err, subscriptions.ErrNoRoute) {
			t.Errorf("a USD subscription with only Razorpay routed: %v", err)
		}
	})
}
