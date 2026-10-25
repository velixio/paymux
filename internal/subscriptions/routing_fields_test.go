package subscriptions_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
)

// F-D: like a payment, a subscription can restrict routing to some connectors
// (allowed_connectors) and name the buyer's country, which routing rules match on.
// Both are kept on the subscription.
func TestSubscriptionRoutingRestrictions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.routeTo("razorpay", "whop")
		inr := func(mut func(*subscriptions.CreateInput)) func(*subscriptions.CreateInput) {
			return func(i *subscriptions.CreateInput) {
				i.Method = ""
				if mut != nil {
					mut(i)
				}
			}
		}

		// Unrestricted: routing order, Razorpay first.
		c := e.mustCreate(inr(nil))
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_managed" || len(sub.AllowedConnectors) != 0 || sub.Country != "" {
			t.Errorf("unrestricted: %+v", sub)
		}

		// Restricted to Whop: Razorpay is skipped, and the restriction is kept.
		c = e.mustCreate(inr(func(i *subscriptions.CreateInput) { i.AllowedConnectors = []string{"whop"}; i.Country = "us" }))
		sub := e.sub(c.Subscription.ID)
		if sub.ProviderAccountID != "pa_whop" || !slices.Equal(sub.AllowedConnectors, []string{"whop"}) || sub.Country != "US" {
			t.Errorf("restricted: %+v", sub)
		}

		// A connector that routing does not offer: no route, at creation.
		if _, err := e.create(inr(func(i *subscriptions.CreateInput) { i.AllowedConnectors = []string{"payu"} })); !errors.Is(err, subscriptions.ErrNoRoute) {
			t.Errorf("a connector outside the routing: %v", err)
		}

		// The country feeds routing rules.
		e.setSetting("routing", `{"rules":[{"name":"US","when":{"country":["US"]},"action":{"kind":"route","targets":[{"connector":"whop"}]}}],
			"catch_all":{"name":"catch_all","when":{},"action":{"kind":"route","targets":[{"connector":"razorpay"}]}}}`)
		c = e.mustCreate(inr(func(i *subscriptions.CreateInput) { i.Country = "US" }))
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_whop" {
			t.Errorf("country US went to %s, want whop", sub.ProviderAccountID)
		}
		c = e.mustCreate(inr(func(i *subscriptions.CreateInput) { i.Country = "IN" }))
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_managed" {
			t.Errorf("country IN went to %s, want razorpay", sub.ProviderAccountID)
		}

		// Nonsense is refused.
		for _, bad := range []func(*subscriptions.CreateInput){
			func(i *subscriptions.CreateInput) { i.Country = "USA" },
			func(i *subscriptions.CreateInput) { i.Country = "1" },
			func(i *subscriptions.CreateInput) { i.AllowedConnectors = []string{""} },
		} {
			if _, err := e.create(inr(bad)); !errors.Is(err, subscriptions.ErrInvalid) {
				t.Errorf("accepted nonsense: %v", err)
			}
		}
	})
}
