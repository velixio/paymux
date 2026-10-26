package stripe

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

func subJSON(status string) map[string]any {
	return map[string]any{"id": "sub_1N1", "object": "subscription", "status": status, "metadata": map[string]string{"paymux_ref": "SUBREF01"},
		"current_period_start": nowUnix - 100, "current_period_end": nowUnix + 2592000,
		"items": map[string]any{"data": []any{map[string]any{"price": map[string]any{"unit_amount": 49900, "currency": "inr"}}}}}
}

func TestSubscriptionCheckoutCreatesAnInlinePrice(t *testing.T) {
	f := newFake()
	f.reply("POST /v1/checkout/sessions", 200, map[string]any{"id": "cs_test_s1", "url": "https://checkout.stripe.com/c/pay/cs_test_s1", "expires_at": nowUnix + 86400})
	c := newConn(t, f)
	res, err := c.CreateSubscriptionCheckout(bg, connector.SubscriptionRequest{SubscriptionID: "sub_x", ProviderRef: "SUBREF01", Amount: 49900, Currency: "INR",
		Interval: "month", IntervalCount: 3, Description: "Pro plan", Customer: connector.Customer{Email: "asha@example.com"}, Metadata: map[string]string{"plan": "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderOrderID != "cs_test_s1" || res.NextAction.Kind != connector.ActionRedirect || res.NextAction.URL != "https://checkout.stripe.com/c/pay/cs_test_s1" {
		t.Fatalf("res = %+v", res)
	}
	connectortest.CheckAllowed(t, c, res.NextAction)
	c0 := f.to("POST /v1/checkout/sessions")[0]
	for k, v := range map[string]string{
		"mode": "subscription", "client_reference_id": "SUBREF01",
		"line_items[0][price_data][unit_amount]": "49900", "line_items[0][price_data][currency]": "inr",
		"line_items[0][price_data][recurring][interval]": "month", "line_items[0][price_data][recurring][interval_count]": "3",
		"line_items[0][price_data][product_data][name]": "Pro plan",
		"metadata[paymux_ref]":                          "SUBREF01", "metadata[paymux_sub]": "sub_x",
		"subscription_data[metadata][paymux_ref]": "SUBREF01", "subscription_data[metadata][paymux_sub]": "sub_x", "subscription_data[metadata][u_plan]": "pro",
		"customer_email": "asha@example.com",
	} {
		if c0.Form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, c0.Form.Get(k), v)
		}
	}
	if c0.Form.Has("subscription_data[trial_end]") || c0.Header.Get("Idempotency-Key") != "subcheckout:SUBREF01" {
		t.Errorf("form = %v", c0.Form)
	}

	// A start far enough ahead is a trial.
	start := time.Unix(nowUnix, 0).Add(10 * 24 * time.Hour)
	if _, err := c.CreateSubscriptionCheckout(bg, connector.SubscriptionRequest{ProviderRef: "SUBREF02", Amount: 100, Currency: "USD", Interval: "month", IntervalCount: 1,
		StartAt: start.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	c1 := f.to("POST /v1/checkout/sessions")[1]
	if c1.Form.Get("subscription_data[trial_end]") != itoa(start.Unix()) {
		t.Errorf("trial_end = %q (want %d)", c1.Form.Get("subscription_data[trial_end]"), start.Unix())
	}
	// Checkout rejects subscription_data[cancel_at] ("Received unknown parameter",
	// verified against the live API), so a plan that ends after N cycles cannot be
	// sold through it: refused up front instead of selling an endless subscription.
	before := f.count()
	if _, err := c.CreateSubscriptionCheckout(bg, connector.SubscriptionRequest{ProviderRef: "SUBREF09", Amount: 100, Currency: "USD", Interval: "month", IntervalCount: 1, TotalCount: 12}); !errors.Is(err, connector.ErrUnsupported) || f.count() != before {
		t.Errorf("total_count: err=%v requests=%d", err, f.count()-before)
	}
	// A start inside the 48 hours Checkout needs is not turned into a trial.
	if _, err := c.CreateSubscriptionCheckout(bg, connector.SubscriptionRequest{ProviderRef: "SUBREF03", Amount: 100, Currency: "USD", Interval: "week", IntervalCount: 1,
		StartAt: time.Unix(nowUnix, 0).Add(24 * time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if f.to("POST /v1/checkout/sessions")[2].Form.Has("subscription_data[trial_end]") {
		t.Error("a start within 48 hours became a trial")
	}
	for name, req := range map[string]connector.SubscriptionRequest{
		"bad interval": {ProviderRef: "r", Amount: 1, Currency: "USD", Interval: "fortnight", IntervalCount: 1},
		"zero count":   {ProviderRef: "r", Amount: 1, Currency: "USD", Interval: "month"},
		"no ref":       {Amount: 1, Currency: "USD", Interval: "month", IntervalCount: 1},
		"zero amount":  {ProviderRef: "r", Currency: "USD", Interval: "month", IntervalCount: 1},
		"bad currency": {ProviderRef: "r", Amount: 1, Currency: "ZZZ", Interval: "month", IntervalCount: 1},
	} {
		if _, err := c.CreateSubscriptionCheckout(bg, req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFetchAndCancelSubscription(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/subscriptions/sub_1N1", 200, subJSON("trialing"))
	f.reply("POST /v1/subscriptions/sub_1N1", 200, subJSON("active"))
	f.reply("DELETE /v1/subscriptions/sub_1N1", 200, subJSON("canceled"))
	c := newConn(t, f)
	s, err := c.FetchSubscription(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_1N1"})
	if err != nil || s.Status != "active" || s.RawStatus != "trialing" || s.ProviderRef != "SUBREF01" || s.Amount != 49900 || s.Currency != "INR" ||
		s.CurrentPeriodEnd != (nowUnix+2592000)*1000 {
		t.Fatalf("sub = %+v %v", s, err)
	}
	// Before the customer has paid the only id is the checkout session's.
	before := f.count()
	if _, err := c.FetchSubscription(bg, connector.SubscriptionRef{ProviderSubscriptionID: "cs_test_s1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("session id: %v", err)
	}
	if err := c.CancelSubscription(bg, connector.SubscriptionRef{ProviderSubscriptionID: "cs_test_s1"}, false); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("cancel before purchase: %v", err)
	}
	if f.count() != before {
		t.Error("a request was made for an id that is not a subscription")
	}
	if err := c.CancelSubscription(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_1N1"}, true); err != nil {
		t.Fatal(err)
	}
	if got := f.to("POST /v1/subscriptions/sub_1N1"); len(got) != 1 || got[0].Form.Get("cancel_at_period_end") != "true" {
		t.Errorf("at period end = %+v", got)
	}
	if err := c.CancelSubscription(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_1N1"}, false); err != nil {
		t.Fatal(err)
	}
	if len(f.to("DELETE /v1/subscriptions/sub_1N1")) != 1 {
		t.Error("immediate cancel did not DELETE")
	}
	f.on("GET /v1/subscriptions/sub_gone", func(w http.ResponseWriter, _ call) {
		stripeErr(w, 404, "invalid_request_error", "resource_missing", "No such subscription")
	})
	if _, err := c.FetchSubscription(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_gone"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("gone: %v", err)
	}
}
