package stripe

import (
	"net/http"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// providerFor serves the session fetch of a payment in the given state.
func providerFor(status, pay, pi string) http.Handler {
	f := newFake()
	var intent any
	if pi != "" {
		intent = intentJSON("pi_3A1", pi, "pay_ref_01", chargeJSON("card", 0))
	}
	f.reply("GET /v1/checkout/sessions/cs_test_1", 200, sessionJSON("cs_test_1", "pay_ref_01", status, pay, intent))
	f.reply("POST /v1/checkout/sessions", 200, sessionJSON("cs_test_1", "pay_ref_01", "open", "unpaid", nil))
	return authed(f)
}

// authed lets the kit's deps through: it builds connectors with testConfig.
func authed(f *fake) http.Handler { return f }

func TestConformance(t *testing.T) {
	var cases []connectortest.StatusCase
	for _, s := range []struct {
		status, pay, pi string
		want            connector.PaymentStatus
	}{
		{"open", "unpaid", "", connector.PaymentRequiresAction},
		{"complete", "paid", "succeeded", connector.PaymentSucceeded},
		{"complete", "unpaid", "processing", connector.PaymentProcessing},
		{"expired", "unpaid", "", connector.PaymentExpired},
		{"open", "unpaid", "requires_capture", connector.PaymentAuthorized},
		{"open", "unpaid", "canceled", connector.PaymentCanceled},
	} {
		cases = append(cases, connectortest.StatusCase{Raw: s.status + "/" + s.pay, Want: s.want, WantAmount: 49900, Handler: providerFor(s.status, s.pay, s.pi)})
	}
	body := fixture(t, "event_checkout_session_completed")
	stale := hook(body, testSecret, nowUnix-3600)
	connectortest.Run(t, New, connectortest.Fixtures{
		Config:   testConfig,
		Now:      time.Unix(nowUnix, 0),
		Ref:      connector.PaymentRef{ProviderOrderID: "cs_test_1", ProviderRef: "pay_ref_01"},
		Statuses: cases,
		Webhooks: connectortest.WebhookFixtures{
			Valid:            hook(body, testSecret, nowUnix),
			WrongSecret:      hook(body, "whsec_not_the_secret", nowUnix),
			SignatureHeaders: []string{"Stripe-Signature"},
			Stale:            &stale,
		},
		// No Return fixtures: Stripe signs nothing on the redirect, so HandleReturn
		// fetches the session (TestHandleReturnChecksTheSessionAgainstTheAttempt).
		Checkout: connector.CheckoutRequest{PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR",
			Customer: connector.Customer{Email: "asha@example.com"}},
		CheckoutHandler: providerFor("open", "unpaid", ""),
		ConnectionOK: func() http.Handler {
			f := newFake()
			f.reply("GET /v1/checkout/sessions", 200, map[string]any{"object": "list", "data": []any{}, "has_more": false})
			return f
		}(),
		ConnectionRejected: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripeErr(w, 401, "invalid_request_error", "", "Invalid API Key provided")
		}),
	})
}
