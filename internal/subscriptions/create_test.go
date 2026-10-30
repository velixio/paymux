package subscriptions_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/subscriptions"
)

var t0 = utc(2027, 1, 31, 4, 0) // 09:30 IST: outside the UPI peak windows

func TestCreateMerchantTriggered(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.sub(c.Subscription.ID)
		if sub.Status != "incomplete" || sub.BillingMode != "merchant_triggered" || sub.ProviderAccountID != "pa_upi" ||
			sub.Amount != 49900 || sub.Interval != "month" || sub.IntervalCount != 1 || !sub.ExpiresAt.Equal(t0.Add(24*3600e9)) {
			t.Fatalf("sub = %+v", sub)
		}
		if !strings.HasPrefix(c.CheckoutURL, "https://pay.example.com/s/"+sub.ID+"?t=") {
			t.Errorf("checkout url = %s", c.CheckoutURL)
		}
		m := e.mandate(sub.MandateID)
		if m.Status != "pending" || m.Method != "upi" || m.MaxAmount != 74850 || m.Currency != "INR" || m.ProviderRef == "" || m.ProviderRef == sub.ProviderRef {
			t.Errorf("mandate = %+v (max is amount x 1.5 headroom)", m)
		}
		if !m.ValidUntil.Equal(t0.AddDate(5, 0, 0)) {
			t.Errorf("valid_until = %v, want 5 years (mandate_validity)", m.ValidUntil)
		}
		if got := e.pendingJobs("subscription.expire_incomplete"); len(got) != 1 || !got[0].Equal(sub.ExpiresAt) {
			t.Errorf("expiry jobs = %v", got)
		}
		if got := e.eventTypes(); strings.Join(got, ",") != "mandate.created,subscription.created" {
			t.Errorf("events = %v", got)
		}
		// The token is stored hashed and opens exactly this subscription.
		if sub.CheckoutTokenHash == "" || strings.Contains(c.CheckoutURL, sub.CheckoutTokenHash) {
			t.Error("token not hashed")
		}
	})
}

func TestCreateValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		bad := map[string]func(*subscriptions.CreateInput){
			"amount":   func(i *subscriptions.CreateInput) { i.Amount = 0 },
			"currency": func(i *subscriptions.CreateInput) { i.Currency = "ZZZ" },
			"interval": func(i *subscriptions.CreateInput) { i.Interval = "fortnight" },
			"count":    func(i *subscriptions.CreateInput) { i.IntervalCount = 9999 },
			"trial":    func(i *subscriptions.CreateInput) { i.TrialDays = -1 },
			"customer": func(i *subscriptions.CreateInput) { i.Customer = nil },
			"method":   func(i *subscriptions.CreateInput) { i.Method = "cheque" },
			"url":      func(i *subscriptions.CreateInput) { i.ReturnURL = "javascript:alert(1)" },
		}
		for name, mut := range bad {
			_, err := e.create(mut)
			if !errors.Is(err, subscriptions.ErrInvalid) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
}

// An amount above the mandate limit falls back to e-NACH, then to a
// provider-managed account, and is refused when neither exists (spec 6.8).
func TestAmountOverTheMandateLimit(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		const big = 2_000_000 // Rs 20,000: above the Rs 15,000 UPI/card limit
		e := newEnv(t, db, t0)

		// Only a UPI/card account: refused with 422 amount_exceeds_mandate_limit.
		_, err := e.create(func(i *subscriptions.CreateInput) { i.Amount = big })
		if !errors.Is(err, subscriptions.ErrAmountExceedsMandateLimit) {
			t.Fatalf("err = %v", err)
		}
		// Exactly at the limit still works, with the max capped at the limit.
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Amount = 1_500_000 })
		if m := e.mandate(e.sub(c.Subscription.ID).MandateID); m.MaxAmount != 1_500_000 {
			t.Errorf("max at the limit = %d", m.MaxAmount)
		}

		// An e-NACH account takes it, ahead of a provider-managed one.
		e.routeTo("payu", "razorpay", "zoho")
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Amount = big; i.Method = "" })
		sub := e.sub(c.Subscription.ID)
		m := e.mandate(sub.MandateID)
		if sub.ProviderAccountID != "pa_enach" || sub.BillingMode != "merchant_triggered" || m.Method != "emandate" || m.MaxAmount != 3_000_000 {
			t.Errorf("e-NACH route: sub=%+v mandate=%+v", sub, m)
		}

		// No e-NACH: the provider-managed account takes it.
		e.routeTo("payu", "razorpay")
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Amount = big; i.Method = "" })
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_managed" || sub.BillingMode != "provider_managed" || sub.MandateID != "" {
			t.Errorf("managed fallback: %+v", sub)
		}

		// Below the limit, routing order decides.
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "" })
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_upi" {
			t.Errorf("order: %s", sub.ProviderAccountID)
		}
		e.routeTo("razorpay", "payu")
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "" })
		if sub := e.sub(c.Subscription.ID); sub.ProviderAccountID != "pa_managed" {
			t.Errorf("order: %s", sub.ProviderAccountID)
		}
	})
}

func TestRoutingErrors(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		// An account that does not have the subscription purpose is not routable.
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE provider_accounts SET enabled_purposes = '["one_time"]'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.create(nil); !errors.Is(err, subscriptions.ErrNoRoute) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTrialAndStartAt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 14 })
		sub := e.sub(c.Subscription.ID)
		if !sub.TrialEnd.Equal(t0.AddDate(0, 0, 14)) || !sub.AnchorAt.Equal(sub.TrialEnd) {
			t.Errorf("trial: %+v", sub)
		}
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.StartAt = utc(2027, 3, 1, 4, 0) })
		if sub := e.sub(c.Subscription.ID); !sub.AnchorAt.Equal(utc(2027, 3, 1, 4, 0)) {
			t.Errorf("start_at: %+v", sub.AnchorAt)
		}
	})
}

func TestIncompleteExpiresAfter24Hours(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		e.advanceTo(t0.Add(23 * 3600e9))
		if s := e.sub(c.Subscription.ID); s.Status != "incomplete" {
			t.Fatalf("expired early: %s", s.Status)
		}
		e.advanceTo(t0.Add(24 * 3600e9))
		sub := e.sub(c.Subscription.ID)
		if sub.Status != "incomplete_expired" {
			t.Fatalf("status = %s", sub.Status)
		}
		if m := e.mandate(sub.MandateID); m.Status != "expired" {
			t.Errorf("mandate = %s", m.Status)
		}
		if len(e.eventsOf("subscription.incomplete_expired")) != 1 {
			t.Errorf("events = %v", e.eventTypes())
		}
		// A late activation of an expired subscription changes nothing.
		e.activateMandateByID(e.mandate(sub.MandateID), "pm_late")
		if s := e.sub(sub.ID); s.Status != "incomplete_expired" {
			t.Errorf("revived: %s", s.Status)
		}
	})
}
