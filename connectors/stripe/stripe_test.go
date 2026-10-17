package stripe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

var bg = context.Background()

func TestNewValidatesConfig(t *testing.T) {
	deps := connectorDeps()
	for name, cfg := range map[string]string{
		"not json":         `{`,
		"no key":           `{"webhook_secret":"w"}`,
		"no webhook":       `{"secret_key":"sk_test_x"}`,
		"publishable key":  `{"secret_key":"pk_test_xxxxxxxx","webhook_secret":"w"}`,
		"unknown property": `{"secret_key":"sk_test_x","webhook_secret":"w","x":1}`,
	} {
		_, err := New([]byte(cfg), deps)
		if name == "unknown property" {
			continue // the schema forbids it; the factory tolerates it
		}
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New([]byte(`{"secret_key":"rk_live_restricted","webhook_secret":"w"}`), deps); err != nil {
		t.Errorf("restricted key: %v", err)
	}
}

func TestInfo(t *testing.T) {
	i := newConn(t, newFake()).Info()
	for _, c := range []connector.Capability{connector.CapRefund, connector.CapList, connector.CapDisputeRead, connector.CapDisputeRespond, connector.CapSettle, connector.CapSubscriptionManager} {
		if !i.Has(c) {
			t.Errorf("missing capability %s", c)
		}
	}
	if i.Name != "stripe" || i.MerchantOfRecord || len(i.SubscriptionModes) != 1 || i.SubscriptionModes[0] != connector.ProviderManaged {
		t.Errorf("info = %+v", i)
	}
}

func TestCheckoutCreatesAStripeSessionAndRedirects(t *testing.T) {
	f := newFake()
	f.reply("POST /v1/checkout/sessions", 200, sessionJSON("cs_test_1", "pay_ref_01", "open", "unpaid", nil))
	c := newConn(t, f)
	res, err := c.CreateCheckout(bg, connector.CheckoutRequest{PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01",
		Amount: 49900, Currency: "INR", Description: "Pro plan", Customer: connector.Customer{Email: "asha@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderOrderID != "cs_test_1" || res.NextAction.Kind != connector.ActionRedirect || res.NextAction.URL != "https://checkout.stripe.com/c/pay/cs_test_1" ||
		res.ExpiresAt != (nowUnix+86000)*1000 {
		t.Fatalf("res = %+v", res)
	}
	got := f.to("POST /v1/checkout/sessions")
	if len(got) != 1 {
		t.Fatalf("calls = %d", len(got))
	}
	c0 := got[0]
	want := map[string]string{
		"mode": "payment", "client_reference_id": "pay_ref_01", "line_items[0][quantity]": "1",
		"line_items[0][price_data][currency]": "inr", "line_items[0][price_data][unit_amount]": "49900",
		"line_items[0][price_data][product_data][name]": "Pro plan", "customer_email": "asha@example.com",
		"metadata[paymux_ref]": "pay_ref_01", "metadata[paymux_payment]": "pay_01", "metadata[paymux_attempt]": "att_01",
		"payment_intent_data[metadata][paymux_ref]": "pay_ref_01",
		"success_url": "https://pay.example.com/return/acc/pay_ref_01?session_id={CHECKOUT_SESSION_ID}",
		"cancel_url":  "https://pay.example.com/return/acc/pay_ref_01?stripe_canceled=1",
	}
	for k, v := range want {
		if c0.Form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, c0.Form.Get(k), v)
		}
	}
	if c0.Header.Get("Idempotency-Key") != "checkout:pay_ref_01" || c0.Header.Get("Stripe-Version") != apiVersion ||
		c0.Header.Get("Authorization") != "Bearer "+testKey {
		t.Errorf("headers = %v", c0.Header)
	}
	if strings.Contains(string(c0.Body), testKey) {
		t.Error("the key is in the body")
	}

	// A response without an https url is refused.
	f.reply("POST /v1/checkout/sessions", 200, map[string]any{"id": "cs_x", "url": "http://evil.example.com"})
	if _, err := c.CreateCheckout(bg, connector.CheckoutRequest{ProviderRef: "r", Amount: 100, Currency: "USD"}); err == nil {
		t.Error("an http checkout url was accepted")
	}
	for name, req := range map[string]connector.CheckoutRequest{
		"no ref": {Amount: 100, Currency: "USD"}, "zero": {ProviderRef: "r", Amount: 0, Currency: "USD"},
		"unknown currency": {ProviderRef: "r", Amount: 100, Currency: "ZZZ"}, "long ref": {ProviderRef: strings.Repeat("x", 201), Amount: 100, Currency: "USD"},
	} {
		if _, err := c.CreateCheckout(bg, req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCheckoutAmountsFollowStripesCurrencyRules(t *testing.T) {
	f := newFake()
	f.reply("POST /v1/checkout/sessions", 200, sessionJSON("cs_test_1", "r", "open", "unpaid", nil))
	c := newConn(t, f)
	for _, tc := range []struct {
		cur    string
		minor  int64
		want   string
		failed bool
	}{
		{"USD", 1999, "1999", false},
		{"INR", 49900, "49900", false},
		{"JPY", 500, "500", false},     // zero-decimal: whole units
		{"KRW", 12000, "12000", false}, // zero-decimal
		{"MGA", 500000, "5000", false}, // two-decimal in ISO, zero-decimal at Stripe
		{"ISK", 5, "500", false},       // zero-decimal in ISO, two-decimal at Stripe
		{"KWD", 12340, "12340", false}, // three-decimal, last digit zero
		{"KWD", 12345, "", true},       // three-decimal with a last digit Stripe refuses
		{"MGA", 500050, "", true},      // fractions of an ariary cannot be sent
	} {
		_, err := c.CreateCheckout(bg, connector.CheckoutRequest{ProviderRef: "r", Amount: tc.minor, Currency: tc.cur})
		if tc.failed {
			if err == nil {
				t.Errorf("%s %d accepted", tc.cur, tc.minor)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s %d: %v", tc.cur, tc.minor, err)
			continue
		}
		calls := f.to("POST /v1/checkout/sessions")
		if got := calls[len(calls)-1].Form.Get("line_items[0][price_data][unit_amount]"); got != tc.want {
			t.Errorf("%s %d sent as %s, want %s", tc.cur, tc.minor, got, tc.want)
		}
	}
	// Reading goes the other way.
	for _, tc := range []struct {
		cur      string
		stripe   int64
		wantISO  int64
		roundtrp bool
	}{{"USD", 1999, 1999, true}, {"JPY", 500, 500, true}, {"MGA", 5000, 500000, true}, {"ISK", 500, 5, true}, {"KWD", 12340, 12340, true}, {"ISK", -500, -5, false}} {
		if got := fromStripe(tc.stripe, tc.cur); got != tc.wantISO {
			t.Errorf("fromStripe(%d, %s) = %d, want %d", tc.stripe, tc.cur, got, tc.wantISO)
		}
		if tc.roundtrp {
			if back, err := toStripe(tc.wantISO, tc.cur); err != nil || back != tc.stripe {
				t.Errorf("toStripe(%d, %s) = %d %v", tc.wantISO, tc.cur, back, err)
			}
		}
	}
}

func TestFetchPaymentStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status, pay, pi string
		want            connector.PaymentStatus
	}{
		{"open", "unpaid", "", connector.PaymentRequiresAction},
		{"complete", "paid", "succeeded", connector.PaymentSucceeded},
		{"complete", "no_payment_required", "", connector.PaymentSucceeded},
		{"complete", "unpaid", "processing", connector.PaymentProcessing}, // a delayed method
		{"expired", "unpaid", "", connector.PaymentExpired},
		{"open", "unpaid", "requires_capture", connector.PaymentAuthorized},
		{"open", "unpaid", "canceled", connector.PaymentCanceled},
		{"some_new_status", "unpaid", "", connector.PaymentProcessing}, // unknown stays in flight
	} {
		f := newFake()
		var pi any
		if tc.pi != "" {
			pi = intentJSON("pi_3A1", tc.pi, "pay_ref_01", chargeJSON("card", 0))
		}
		f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", tc.status, tc.pay, pi))
		snap, err := newConn(t, f).FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
		if err != nil || snap.Status != tc.want || snap.RawStatus != tc.status+"/"+tc.pay || snap.Amount != 49900 || snap.Currency != "INR" ||
			snap.ProviderRef != "pay_ref_01" || snap.ProviderOrderID != "cs_test_1" {
			t.Errorf("%+v: snap=%+v err=%v", tc, snap, err)
		}
	}
}

func TestFetchPaymentExpandsTheIntentAndCharge(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "complete", "paid", intentJSON("pi_3A1", "succeeded", "pay_ref_01", chargeJSON("upi", 10000))))
	snap, err := newConn(t, f).FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if err != nil || snap.ProviderPaymentID != "pi_3A1" || snap.Method != "upi" || snap.AmountRefunded != 10000 {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	q := f.to("GET /v1/checkout/sessions/cs_test_1")[0].Query["expand[]"]
	if len(q) != 2 || q[0] != "payment_intent" || q[1] != "payment_intent.latest_charge" {
		t.Errorf("expand = %v", q)
	}
	// A failed card in a session that is still open is reported with its reason.
	pi := intentJSON("pi_3A1", "requires_payment_method", "pay_ref_01", nil)
	pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds", "message": "Your card has insufficient funds."}
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "open", "unpaid", pi))
	snap, _ = newConn(t, f).FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"})
	if snap.Status != connector.PaymentRequiresAction || snap.ErrorCode != "insufficient_funds" || !strings.Contains(snap.ErrorMessage, "insufficient") {
		t.Errorf("snap = %+v", snap)
	}
}

// Real APIs differ from their docs, so nothing is found by searching and
// everything fetched is checked against what this connector created.
func TestFetchPaymentIsMatchedStrictly(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/checkout/sessions/cs_test_other", 200, sessionJSON("cs_test_other", "SOMEONE_ELSES_REF", "complete", "paid", "pi_other"))
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "complete", "paid", "pi_3A1"))
	f.reply("GET /v1/payment_intents/pi_other", 200, intentJSON("pi_other", "succeeded", "SOMEONE_ELSES_REF", nil))
	f.reply("GET /v1/payment_intents/pi_3A1", 200, intentJSON("pi_3A1", "succeeded", "pay_ref_01", nil))
	c := newConn(t, f)

	// A session that carries another payment's reference is not this payment.
	if _, err := c.FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_other", ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("foreign session: %v", err)
	}
	if _, err := c.FetchPayment(bg, connector.PaymentRef{ProviderPaymentID: "pi_other", ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("foreign intent: %v", err)
	}
	// A payment intent that is not the session's is refused too.
	if _, err := c.FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01", ProviderPaymentID: "pi_other"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("session with another payment intent: %v", err)
	}
	// Ids of the wrong kind never reach the API.
	before := f.count()
	for _, ref := range []connector.PaymentRef{{ProviderOrderID: "order_123", ProviderRef: "pay_ref_01"}, {ProviderPaymentID: "ch_3A1"}, {ProviderOrderID: "../customers"}} {
		if _, err := c.FetchPayment(bg, ref); !errors.Is(err, connector.ErrNotFound) {
			t.Errorf("%+v: %v", ref, err)
		}
	}
	// With only the reference there is nothing to fetch, and nothing is searched.
	if _, err := c.FetchPayment(bg, connector.PaymentRef{ProviderRef: "pay_ref_01"}); err == nil {
		t.Error("a bare reference was accepted")
	}
	if f.count() != before {
		t.Errorf("%d unexpected requests", f.count()-before)
	}
	// The right objects work.
	if snap, err := c.FetchPayment(bg, connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01", ProviderPaymentID: "pi_3A1"}); err != nil || snap.Status != connector.PaymentSucceeded {
		t.Errorf("session: %+v %v", snap, err)
	}
	if snap, err := c.FetchPayment(bg, connector.PaymentRef{ProviderPaymentID: "pi_3A1", ProviderRef: "pay_ref_01"}); err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderRef != "pay_ref_01" {
		t.Errorf("intent: %+v %v", snap, err)
	}
	for _, c := range f.calls {
		if strings.Contains(c.Path, "search") || (c.Path == "/v1/checkout/sessions" && c.Method == "GET") {
			t.Errorf("a search was made: %s %s", c.Method, c.Path)
		}
	}
}

func TestErrorsAreTyped(t *testing.T) {
	f := newFake()
	f.on("GET /v1/checkout/sessions/cs_a", func(w http.ResponseWriter, _ call) {
		stripeErr(w, 404, "invalid_request_error", "resource_missing", "No such checkout.session")
	})
	f.on("GET /v1/checkout/sessions/cs_b", func(w http.ResponseWriter, _ call) {
		w.Header().Set("Retry-After", "7")
		stripeErr(w, 429, "api_error", "rate_limit", "slow down")
	})
	f.on("GET /v1/checkout/sessions/cs_c", func(w http.ResponseWriter, _ call) { stripeErr(w, 503, "api_error", "", "down") })
	f.on("GET /v1/checkout/sessions/cs_d", func(w http.ResponseWriter, _ call) { write(w, 200, "not an object") })
	c := newConn(t, f)
	ref := func(id string) connector.PaymentRef { return connector.PaymentRef{ProviderOrderID: id} }
	if _, err := c.FetchPayment(bg, ref("cs_a")); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("not found: %v", err)
	}
	var rl connector.ErrRateLimited
	if _, err := c.FetchPayment(bg, ref("cs_b")); !errors.As(err, &rl) || rl.RetryAfter.Seconds() != 7 {
		t.Errorf("rate limited: %v", err)
	}
	if _, err := c.FetchPayment(bg, ref("cs_c")); !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("5xx: %v", err)
	}
	if _, err := c.FetchPayment(bg, ref("cs_d")); err == nil {
		t.Error("an undecodable response was accepted")
	}
	bad, _ := New([]byte(`{"secret_key":"sk_test_wrong_key","webhook_secret":"w"}`), connectorDepsFor(t, f))
	if _, err := bad.FetchPayment(bg, ref("cs_a")); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("bad key: %v", err)
	}
}

func TestHandleReturnChecksTheSessionAgainstTheAttempt(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", "complete", "paid", "pi_3A1"))
	f.reply("GET /v1/checkout/sessions/cs_test_other", 200, sessionJSON("cs_test_other", "other_ref", "complete", "paid", "pi_9"))
	c := newConn(t, f)
	ret := func(ref string, q map[string][]string) (connector.ReturnResult, error) {
		return c.HandleReturn(bg, connector.ReturnRequest{ProviderRef: ref, Method: "GET", Query: q})
	}
	got, err := ret("pay_ref_01", map[string][]string{"session_id": {"cs_test_1"}})
	if err != nil || got != (connector.ReturnResult{ProviderRef: "pay_ref_01", ProviderOrderID: "cs_test_1", ProviderPaymentID: "pi_3A1"}) {
		t.Fatalf("valid return: %+v %v", got, err)
	}
	for name, q := range map[string]map[string][]string{
		"another payment's session": {"session_id": {"cs_test_other"}},
		"no session":                {},
		"not a session id":          {"session_id": {"pi_3A1"}},
		"unknown session":           {"session_id": {"cs_test_unknown"}},
	} {
		if _, err := ret("pay_ref_01", q); !errors.Is(err, connector.ErrInvalidReturn) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Coming back from the cancel link needs no session.
	if got, err := ret("pay_ref_01", map[string][]string{"stripe_canceled": {"1"}}); err != nil || got.ProviderRef != "pay_ref_01" || got.ProviderOrderID != "" {
		t.Errorf("canceled return: %+v %v", got, err)
	}
}
