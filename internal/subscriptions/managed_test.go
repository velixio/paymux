package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/connectors/razorpay"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// rzp parses a Razorpay webhook fixture with the real connector, the way the
// inbound job would.
func rzp(t *testing.T, name string) []connector.Notification {
	t.Helper()
	body, err := os.ReadFile("../../connectors/razorpay/testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	cfg := json.RawMessage(`{"key_id":"k","key_secret":"s","webhook_secret":"w"}`)
	c, err := razorpay.New(cfg, connectortest.NewDeps(nil, time.Unix(1760000100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Body: body,
		Headers: map[string][]string{"X-Razorpay-Event-Id": {"evt_" + name}}})
	if err != nil || len(ns) != 1 {
		t.Fatalf("%s: %v %v", name, ns, err)
	}
	return ns
}

// managed creates a provider-managed subscription already linked to the
// provider subscription id used in the Razorpay fixtures (sub_Nx5).
func managed(t *testing.T, e *env) store.Subscription {
	t.Helper()
	e.routeTo("razorpay")
	c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "" })
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE subscriptions SET provider_subscription_id = 'sub_Nx5' WHERE id = ?", c.Subscription.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e.sub(c.Subscription.ID)
}

func TestRazorpayProviderManagedRenewalViaWebhook(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := managed(t, e)
		if sub.BillingMode != "provider_managed" || sub.MandateID != "" {
			t.Fatalf("sub = %+v", sub)
		}
		note := func(name string) {
			t.Helper()
			for _, n := range rzp(t, name) {
				if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
		}

		note("event_subscription_activated")
		if got := e.sub(sub.ID); got.Status != "active" || len(e.eventsOf("subscription.activated")) != 1 {
			t.Fatalf("activated: %+v events=%v", got, e.eventTypes())
		}

		// subscription.charged: a renewal payment with a captured attempt, invoiced.
		note("event_subscription_charged")
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Kind != "renewal" || pays[0].Status != "succeeded" || pays[0].Amount != 49900 || pays[0].AmountReceived != 49900 {
			t.Fatalf("payments = %+v", pays)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			atts, _ := store.ListAttempts(ctx, q, pays[0].ID)
			if len(atts) != 1 || atts[0].Status != "captured" || atts[0].ProviderPaymentID != "pay_Nx1" || atts[0].ProviderAccountID != "pa_managed" {
				t.Errorf("attempts = %+v", atts)
			}
			invs, _ := store.InvoicesForPayment(ctx, q, pays[0].ID)
			if len(invs) != 1 || invs[0].Total != 49900 {
				t.Errorf("invoices = %+v", invs)
			}
			return nil
		})
		got := e.sub(sub.ID)
		if got.RenewalsDone != 1 || got.Status != "active" || !got.CurrentPeriodEnd.Equal(time.Unix(1762592000, 0).UTC()) ||
			len(e.eventsOf("subscription.renewed")) != 1 {
			t.Errorf("sub = %+v", got)
		}

		// The same delivery again changes nothing.
		note("event_subscription_charged")
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 {
			t.Errorf("duplicate delivery created %d payments", len(pays))
		}
		if got := e.sub(sub.ID); got.RenewalsDone != 1 {
			t.Errorf("renewals = %d", got.RenewalsDone)
		}

		// pending/halted: past due; a later charge brings it back.
		note("event_subscription_halted")
		if got := e.sub(sub.ID); got.Status != "past_due" || len(e.eventsOf("subscription.past_due")) != 1 {
			t.Errorf("halted: %+v", got)
		}
		// completed: the provider's last cycle is done.
		note("event_subscription_completed")
		if got := e.sub(sub.ID); got.Status != "ended" {
			t.Errorf("completed: %+v", got)
		}
		// Nothing revives a finished subscription.
		note("event_subscription_activated")
		if got := e.sub(sub.ID); got.Status != "ended" {
			t.Errorf("revived: %+v", got)
		}
	})
}

func TestRazorpayChargeBeforeActivationActivates(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := managed(t, e)
		for _, n := range rzp(t, "event_subscription_charged") { // arrives before "activated"
			if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.sub(sub.ID); got.Status != "active" || got.RenewalsDone != 1 {
			t.Errorf("sub = %+v", got)
		}
	})
}

func TestRazorpayCancelledAndPending(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := managed(t, e)
		for _, name := range []string{"event_subscription_activated", "event_subscription_pending"} {
			for _, n := range rzp(t, name) {
				if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := e.sub(sub.ID); got.Status != "past_due" {
			t.Fatalf("pending: %s", got.Status)
		}
		for _, n := range rzp(t, "event_subscription_cancelled") {
			if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.sub(sub.ID); got.Status != "canceled" {
			t.Fatalf("cancelled: %s", got.Status)
		}
		// Unknown provider subscription: not found, so the inbound job retries.
		n := rzp(t, "event_subscription_activated")[0]
		n.Subscription.ProviderSubscriptionID = "sub_unknown"
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); err == nil {
			t.Error("unknown subscription accepted")
		}
	})
}

func TestProviderManagedCancelAndAmountGoThroughTheProvider(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := managed(t, e)
		for _, n := range rzp(t, "event_subscription_activated") {
			if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
				t.Fatal(err)
			}
		}
		res, err := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 79900)
		if err != nil || res.Subscription.PendingAmount != 79900 || res.CheckoutURL != "" || len(e.r.updated) != 1 || e.r.updated[0] != 79900 {
			t.Fatalf("update: %+v %v updated=%v", res, err, e.r.updated)
		}
		if _, err := e.subs.Cancel(ctx, e.scope, sub.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := e.subs.Cancel(ctx, e.scope, sub.ID, false); err != nil {
			t.Fatal(err)
		}
		e.runJobs()
		got := append([]string(nil), e.r.canceled...)
		slices.Sort(got)
		if strings.Join(got, ",") != "sub_Nx5:false,sub_Nx5:true" {
			t.Errorf("provider cancels = %v", e.r.canceled)
		}
		if got := e.sub(sub.ID); got.Status != "canceled" {
			t.Errorf("status = %s", got.Status)
		}
	})
}

func hostedServer(e *env) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /s/{id}", e.subs.ServeHosted)
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res.StatusCode, sb.String(), res.Header
}

func TestHostedCheckoutMerchantTriggered(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := hostedServer(e)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		target := srv.URL + u.RequestURI()

		code, body, hdr := fetch(t, target)
		if code != 200 || !strings.Contains(body, `action="https://pg.example.com/si"`) || !strings.Contains(body, `name="txnid"`) ||
			!strings.Contains(body, `name="max" value="74850"`) || hdr.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("form page: %d %s", code, body)
		}
		m := e.mandate(e.sub(c.Subscription.ID).MandateID)
		if len(e.r.mandateReqs) != 1 || e.r.mandateReqs[0].ProviderRef != m.ProviderRef || e.r.mandateReqs[0].Method != "upi" ||
			e.r.mandateReqs[0].Customer.Email != "asha@example.com" {
			t.Fatalf("mandate requests = %+v", e.r.mandateReqs)
		}
		// Reloading reuses the checkout; it does not register a second mandate.
		if code, _, _ := fetch(t, target); code != 200 || len(e.r.mandateReqs) != 1 {
			t.Errorf("reload: %d requests=%d", code, len(e.r.mandateReqs))
		}
		for _, bad := range []string{"/s/" + c.Subscription.ID, "/s/" + c.Subscription.ID + "?t=wrong", "/s/sub_nosuch?t=x"} {
			if code, _, _ := fetch(t, srv.URL+bad); code != 404 {
				t.Errorf("%s -> %d", bad, code)
			}
		}
		// Once active, the link only says so.
		e.activateMandate(c)
		if code, body, _ := fetch(t, target); code != 200 || !strings.Contains(body, "already set up") {
			t.Errorf("active: %d %s", code, body)
		}
		if _, err := e.subs.Cancel(ctx, e.scope, c.Subscription.ID, false); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := fetch(t, target); code != 200 || !strings.Contains(body, "no longer open") {
			t.Errorf("canceled: %d %s", code, body)
		}
	})
}

func TestHostedCheckoutProviderManagedSDK(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := hostedServer(e)
		e.routeTo("razorpay")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = ""; i.TotalCount = 12 })
		u, _ := url.Parse(c.CheckoutURL)
		code, body, _ := fetch(t, srv.URL+u.RequestURI())
		if code != 200 || !strings.Contains(body, `src="https://checkout.example.com/c.js"`) || !strings.Contains(body, "sub_prov_1") {
			t.Fatalf("sdk page: %d %s", code, body)
		}
		fetch(t, srv.URL+u.RequestURI())
		if len(e.r.subReqs) != 1 {
			t.Fatalf("provider subscriptions created = %d, want 1", len(e.r.subReqs))
		}
		req := e.r.subReqs[0]
		if req.Amount != 49900 || req.Interval != "month" || req.IntervalCount != 1 || req.TotalCount != 12 || req.ProviderRef != e.sub(c.Subscription.ID).ProviderRef {
			t.Errorf("request = %+v", req)
		}
		if got := e.sub(c.Subscription.ID); got.ProviderSubscriptionID != "sub_prov_1" {
			t.Errorf("provider id = %q", got.ProviderSubscriptionID)
		}
	})
}

// The hosted subscription page carries a form that moves money to the
// provider; it must not be frameable by another site (clickjacking).
func TestHostedCheckoutRefusesToBeFramed(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := hostedServer(e)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		_, _, hdr := fetch(t, srv.URL+u.RequestURI())
		if hdr.Get("X-Frame-Options") != "DENY" || !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers = %v", hdr)
		}
		// A page that 404s carries them too.
		_, _, hdr = fetch(t, srv.URL+"/s/sub_nosuch?t=x")
		if hdr.Get("X-Frame-Options") != "DENY" {
			t.Errorf("404 headers = %v", hdr)
		}
	})
}
