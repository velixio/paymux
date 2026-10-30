package subscriptions_test

import (
	"net/url"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

func (e *env) failMandate(m store.Mandate, providerID string) {
	e.t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE mandates SET provider_mandate_id = ? WHERE id = ?", providerID, m.ID)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	e.providerSays(providerID, connector.MandateSnapshot{ProviderMandateID: providerID, Status: "failed", RawStatus: "failure"})
	err := e.subs.HandleNotification(ctx, m.ProviderAccountID, connector.Notification{Kind: connector.NotifyMandate, EventID: "f_" + providerID,
		Mandate: &connector.MandateSnapshot{ProviderMandateID: providerID, Status: "failed", RawStatus: "failure"}})
	if err != nil {
		e.t.Fatal(err)
	}
}

// A failed registration is retried on the same subscription with a fresh
// provider reference: providers reject a txnid they have already seen.
func TestRegistrationRetryGetsAFreshMandateReference(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := hostedServer(e)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		target := srv.URL + u.RequestURI()
		sub := e.sub(c.Subscription.ID)
		first := e.mandate(sub.MandateID)
		if code, _, _ := fetch(t, target); code != 200 {
			t.Fatalf("first load: %d", code)
		}
		if len(e.r.mandateReqs) != 1 || e.r.mandateReqs[0].ProviderRef != first.ProviderRef || e.r.mandateReqs[0].Amount != 49900 {
			t.Fatalf("requests = %+v", e.r.mandateReqs)
		}

		e.failMandate(first, "pm_failed")
		if code, _, _ := fetch(t, target); code != 200 {
			t.Fatalf("retry load: %d", code)
		}
		got := e.sub(sub.ID)
		if got.ID != sub.ID || got.Status != "incomplete" || got.MandateID == first.ID {
			t.Fatalf("sub = %+v (same subscription, new mandate)", got)
		}
		second := e.mandate(got.MandateID)
		if second.ProviderRef == first.ProviderRef || second.Status != "pending" || second.Method != "upi" || second.MaxAmount != first.MaxAmount {
			t.Errorf("second mandate = %+v", second)
		}
		if len(e.r.mandateReqs) != 2 || e.r.mandateReqs[1].ProviderRef != second.ProviderRef || e.r.mandateReqs[1].MandateID != second.ID {
			t.Fatalf("requests = %+v", e.r.mandateReqs)
		}
		if e.mandate(first.ID).Status != "failed" {
			t.Errorf("the failed mandate must stay failed: %s", e.mandate(first.ID).Status)
		}
		// Reloading the page does not register a third one.
		fetch(t, target)
		if len(e.r.mandateReqs) != 2 {
			t.Errorf("requests after a reload = %d", len(e.r.mandateReqs))
		}
		// A stale confirmation of the old reference cannot activate it; the new one does.
		e.activateMandateByID(first, "pm_failed")
		if e.sub(sub.ID).Status != "incomplete" {
			t.Fatal("the failed registration activated the subscription")
		}
		e.activateMandateByID(second, "pm_second")
		if got := e.sub(sub.ID); got.Status != "active" || got.MandateID != second.ID {
			t.Errorf("after the retry: %+v", got)
		}
	})
}
