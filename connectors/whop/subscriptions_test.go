package whop

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

func subMgr(t *testing.T, f *fake) connector.SubscriptionManager {
	t.Helper()
	m, ok := newConn(t, f).(connector.SubscriptionManager)
	if !ok {
		t.Fatal("whop does not implement SubscriptionManager")
	}
	return m
}

func TestInfoDeclaresMemberships(t *testing.T) {
	i := newConn(t, newFake("")).Info()
	if !i.Has(connector.CapSubscriptionManager) || len(i.SubscriptionModes) != 1 || i.SubscriptionModes[0] != connector.ProviderManaged || !i.MerchantOfRecord {
		t.Errorf("info = %+v", i)
	}
}

func TestCreateSubscriptionCheckoutBuildsARenewalVariant(t *testing.T) {
	f := newFake("")
	m := subMgr(t, f)
	res, err := m.CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{SubscriptionID: "sub_x", ProviderRef: "SUBREF01",
		Amount: 49900, Currency: "BDT", Interval: "month", IntervalCount: 2, Customer: connector.Customer{Email: "a@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderOrderID != "ch_1" || res.NextAction.Kind != connector.ActionRedirect || res.NextAction.URL != "https://whop.com/checkout/ch_1" {
		t.Fatalf("res = %+v", res)
	}
	connectortest.CheckAllowed(t, m, res.NextAction)
	var body struct {
		AccountID string `json:"account_id"`
		Variant   struct {
			AccountID, BillingType, Currency string
			RenewalPrice, InitialPrice       json.Number
			BillingPeriod                    int
			TrialDays                        *int `json:"trial_period_days"`
		} `json:"variant"`
		Metadata    map[string]string `json:"metadata"`
		RedirectURL string            `json:"redirect_url"`
	}
	raw := f.bodies["POST /api/v1/checkout_configurations"]
	_ = json.Unmarshal(raw, &body)
	p := body.Variant
	// json field names are snake_case in the body; re-decode them explicitly.
	var generic map[string]map[string]any
	_ = json.Unmarshal(raw, &generic)
	pl := generic["variant"]
	if body.AccountID != "biz_1" || pl["billing_type"] != "renewal" || pl["currency"] != "bdt" || pl["account_id"] != "biz_1" || pl["billing_period"] != float64(60) ||
		pl["renewal_price"] != 499.0 || pl["trial_period_days"] != nil {
		t.Errorf("variant = %v (%+v)", pl, p)
	}
	// A renewal variant with an initial_price charges it on top of the first renewal
	// ("Due today $19.98" for a $9.99 variant, seen in the sandbox): none is sent, and
	// the first payment is the renewal price.
	if _, has := pl["initial_price"]; has {
		t.Errorf("initial_price must not be sent for a variant without a trial: %v", pl)
	}
	if body.Metadata["sub"] != "sub_x" || body.Metadata["ref"] != "SUBREF01" || body.RedirectURL != "https://pay.example.com/return/acc/SUBREF01" {
		t.Errorf("body = %s", raw)
	}

	// A start in the future is a trial: nothing is charged until it ends.
	f2 := newFake("")
	start := time.Unix(nowUnix, 0).Add(14*24*time.Hour - time.Hour) // just under 14 days: rounds up
	if _, err := subMgr(t, f2).CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{ProviderRef: "R", Amount: 100,
		Currency: "USD", Interval: "week", IntervalCount: 1, StartAt: start.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(f2.bodies["POST /api/v1/checkout_configurations"], &generic)
	if pl := generic["variant"]; pl["trial_period_days"] != float64(14) || pl["initial_price"] != 0.0 || pl["billing_period"] != float64(7) {
		t.Errorf("trial variant = %v", pl)
	}
	for _, bad := range []connector.SubscriptionRequest{
		{ProviderRef: "R", Amount: 100, Currency: "USD", Interval: "fortnight", IntervalCount: 1},
		{ProviderRef: "R", Amount: 0, Currency: "USD", Interval: "month", IntervalCount: 1},
		{ProviderRef: "", Amount: 1, Currency: "USD", Interval: "month", IntervalCount: 1},
		{ProviderRef: "R", Amount: 1, Currency: "ZZZ", Interval: "month", IntervalCount: 1},
		{ProviderRef: "R", Amount: 1, Currency: "USD", Interval: "month", IntervalCount: 0},
	} {
		if _, err := subMgr(t, newFake("")).CreateSubscriptionCheckout(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestFetchAndCancelMembership(t *testing.T) {
	f := newFake("")
	m := subMgr(t, f)
	for raw, want := range map[string]string{"active": "active", "trialing": "active", "past_due": "past_due", "unresolved": "past_due",
		"canceled": "canceled", "expired": "canceled", "completed": "ended", "drafted": "incomplete", "weird": "active"} {
		f.memberStatus = raw
		s, err := m.FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "mem_w1"})
		if err != nil || s.Status != want || s.RawStatus != raw || s.ProviderRef != "SUBREF01" ||
			s.CurrentPeriodStart != time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC).UnixMilli() {
			t.Errorf("%s: %+v %v", raw, s, err)
		}
	}
	// Before the customer buys, the only id is the checkout configuration's.
	if _, err := m.FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "ch_1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("checkout id: %v", err)
	}
	if err := m.CancelSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "mem_w1"}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.CancelSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "mem_w1"}, false); err != nil {
		t.Fatal(err)
	}
	var b map[string]string
	_ = json.Unmarshal(f.bodies["POST /api/v1/memberships/mem_w1/cancel"], &b)
	if b["cancellation_mode"] != "immediate" {
		t.Errorf("last body = %v", b)
	}
	if err := m.CancelSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "ch_1"}, false); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("cancel before purchase: %v", err)
	}
	if err := m.UpdateSubscriptionAmount(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "mem_w1"}, 1); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("update: %v", err)
	}
}

func TestMembershipAndRenewalWebhooks(t *testing.T) {
	c := newConn(t, newFake(""))
	parse := func(name, id string) connector.Notification {
		t.Helper()
		ns, err := c.ParseWebhook(context.Background(), webhook(t, name, id, []byte(testSecret), nowUnix))
		if err != nil || len(ns) != 1 {
			t.Fatalf("%s: %v %v", name, ns, err)
		}
		return ns[0]
	}
	n := parse("membership_activated", "msg_m1")
	s := n.Subscription
	if n.Kind != connector.NotifySubscription || n.EventType != "membership.activated" || n.Payment != nil || s.ProviderSubscriptionID != "mem_w1" ||
		s.ProviderRef != "SUBREF01" || s.Status != "active" || s.CurrentPeriodEnd != time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("activated = %+v / %+v", n, s)
	}
	if s := parse("membership_canceled", "msg_m2").Subscription; s.Status != "canceled" || s.RawStatus != "canceled" {
		t.Errorf("canceled = %+v", s)
	}
	if s := parse("membership_past_due", "msg_m3").Subscription; s.Status != "past_due" {
		t.Errorf("past due = %+v", s)
	}

	// A renewal payment of a membership: a subscription notification with the payment.
	n = parse("payment_renewal_succeeded", "msg_p1")
	if n.Kind != connector.NotifySubscription || n.Subscription.ProviderSubscriptionID != "mem_w1" || n.Subscription.ProviderRef != "SUBREF01" ||
		n.Subscription.Status != "active" || n.Payment == nil || n.Payment.Status != connector.PaymentSucceeded || n.Payment.Amount != 499 ||
		n.Payment.ProviderPaymentID != "pay_wr1" || n.Payment.Currency != "USD" {
		t.Errorf("renewal = %+v / %+v / %+v", n, n.Subscription, n.Payment)
	}
	if n := parse("payment_renewal_failed", "msg_p2"); n.Subscription.Status != "past_due" || n.Payment.Status != connector.PaymentFailed {
		t.Errorf("failed renewal = %+v / %+v", n.Subscription, n.Payment)
	}
	// A one-time payment (no subscription metadata) is still a plain payment notification.
	if n := parse("payment_succeeded", "msg_2Kx"); n.Kind != connector.NotifyPayment || n.Subscription != nil {
		t.Errorf("one-time = %+v", n)
	}
}

// Current Whop checkout configurations accept an inline variant. The title is
// shown at checkout and must respect Whop's 30-character limit.
func TestRenewalVariantCarriesBoundedTitle(t *testing.T) {
	f := newFake("")
	if _, err := subMgr(t, f).CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{SubscriptionID: "sub_x", ProviderRef: "SUBREF01",
		Amount: 49900, Currency: "BDT", Interval: "month", IntervalCount: 2, Description: "Voicestudio Pro"}); err != nil {
		t.Fatal(err)
	}
	var generic map[string]map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/checkout_configurations"], &generic)
	variant := generic["variant"]
	if variant["title"] != "Voicestudio Pro" {
		t.Fatalf("variant = %v", variant)
	}
	f2 := newFake("")
	if _, err := subMgr(t, f2).CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{ProviderRef: "R", Amount: 100,
		Currency: "USD", Interval: "week", IntervalCount: 1}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(f2.bodies["POST /api/v1/checkout_configurations"], &generic)
	if variant = generic["variant"]; variant["title"] != "Subscription" {
		t.Fatalf("default title: %v", variant)
	}
}

func TestOneTimeVariantCarriesBoundedTitle(t *testing.T) {
	f := newFake("")
	c := newConn(t, f)
	if _, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "p", AttemptID: "a", AppID: "app_1", ProviderRef: "R1",
		Amount: 1000, Currency: "USD", Description: "Supermetry Pro"}); err != nil {
		t.Fatal(err)
	}
	var generic map[string]map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/checkout_configurations"], &generic)
	variant := generic["variant"]
	if variant["title"] != "Supermetry Pro" || variant["billing_type"] != "one_time" {
		t.Fatalf("variant = %v", variant)
	}
	f2 := newFake("")
	if _, err := newConn(t, f2).CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "p2", AttemptID: "a2", AppID: "app_1", ProviderRef: "R2",
		Amount: 2500, Currency: "USD", Description: "A voice product title that is much too long for Whop"}); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(f2.bodies["POST /api/v1/checkout_configurations"], &generic)
	if title, _ := generic["variant"]["title"].(string); len([]rune(title)) != 30 {
		t.Fatalf("bounded title = %q", title)
	}
}
