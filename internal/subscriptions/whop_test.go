package subscriptions_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/connectors/whop"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// whopNotes parses a Whop webhook fixture with the real connector. The fixture
// is rewritten to the subscription's provider_ref and to INR amounts.
func whopNotes(t *testing.T, name, ref string) []connector.Notification {
	t.Helper()
	raw, err := os.ReadFile("../../connectors/whop/testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReplacer("SUBREF01", ref, `"total":4.99`, `"total":499`, `"currency":"usd"`, `"currency":"inr"`).Replace(string(raw))
	c, err := whop.New([]byte(`{"api_key":"k","company_id":"biz_1","webhook_secret":"w"}`), connectortest.NewDeps(nil, time.Unix(1760000100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Body: []byte(body)})
	if err != nil || len(ns) != 1 {
		t.Fatalf("%s: %v %v", name, ns, err)
	}
	return ns
}

func TestWhopMembershipLinksByRefAndRenews(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.routeTo("whop")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "" })
		sub := e.sub(c.Subscription.ID)
		if sub.BillingMode != "provider_managed" || sub.ProviderAccountID != "pa_whop" || sub.ProviderSubscriptionID != "" {
			t.Fatalf("sub = %+v", sub)
		}
		note := func(name string) {
			t.Helper()
			for _, n := range whopNotes(t, name, sub.ProviderRef) {
				if err := e.notify("pa_whop", n); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
		}

		// The membership id is unknown until the customer buys: the ref links it.
		note("membership_activated")
		got := e.sub(sub.ID)
		if got.Status != "active" || got.ProviderSubscriptionID != "mem_w1" || len(e.eventsOf("subscription.activated")) != 1 {
			t.Fatalf("activated: %+v events=%v", got, e.eventTypes())
		}

		// A renewal payment: recorded as a renewal, but Whop is merchant of record: no invoice.
		note("payment_renewal_succeeded")
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Status != "succeeded" || pays[0].Amount != 49900 {
			t.Fatalf("payments = %+v", pays)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			if invs, _ := store.InvoicesForPayment(ctx, q, pays[0].ID); len(invs) != 0 {
				t.Errorf("merchant of record: invoices = %+v", invs)
			}
			return nil
		})
		note("payment_renewal_succeeded")
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 {
			t.Errorf("duplicate delivery created %d payments", len(pays))
		}

		note("payment_renewal_failed")
		note("membership_past_due")
		if got := e.sub(sub.ID); got.Status != "past_due" {
			t.Errorf("past due: %+v", got)
		}
		note("membership_canceled")
		if got := e.sub(sub.ID); got.Status != "canceled" {
			t.Errorf("canceled: %+v", got)
		}
	})
}
