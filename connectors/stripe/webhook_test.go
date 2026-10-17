package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.", ts)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// hook is a webhook delivery received at nowUnix, signed at ts.
func hook(body []byte, secret string, ts int64) connector.WebhookRequest {
	return connector.WebhookRequest{Method: "POST", Body: body, ReceivedAt: nowUnix * 1000,
		Headers: map[string][]string{"Stripe-Signature": {fmt.Sprintf("t=%d,v1=%s", ts, sign(secret, ts, body))}}}
}

// Computed independently with Python hmac over the fixture: "1760000000." + body.
func TestGoldenSignature(t *testing.T) {
	if got := sign(testSecret, 1760000000, fixture(t, "event_checkout_session_completed")); got != "eec4b55194222ed23bf66fd26726ba9afd7a95435165970e5955eea7ddba58e5" {
		t.Fatalf("signature drifted: %s", got)
	}
}

func TestVerifyWebhook(t *testing.T) {
	c := newConn(t, newFake())
	body := fixture(t, "event_checkout_session_completed")
	ok := hook(body, testSecret, nowUnix)
	if err := c.VerifyWebhook(bg, ok); err != nil {
		t.Fatalf("valid: %v", err)
	}
	with := func(h string) connector.WebhookRequest {
		r := ok
		r.Headers = map[string][]string{"Stripe-Signature": {h}}
		return r
	}
	good := sign(testSecret, nowUnix, body)
	otherTS := nowUnix - 1
	for name, req := range map[string]connector.WebhookRequest{
		"wrong secret":     hook(body, "whsec_other", nowUnix),
		"tampered body":    func() connector.WebhookRequest { r := ok; r.Body = append(append([]byte(nil), body...), ' '); return r }(),
		"stale (6 min)":    hook(body, testSecret, nowUnix-6*60),
		"from the future":  hook(body, testSecret, nowUnix+6*60),
		"timestamp swap":   with(fmt.Sprintf("t=%d,v1=%s", otherTS, good)),
		"no header":        func() connector.WebhookRequest { r := ok; r.Headers = nil; return r }(),
		"empty header":     with(""),
		"no v1":            with(fmt.Sprintf("t=%d", nowUnix)),
		"v0 only":          with(fmt.Sprintf("t=%d,v0=%s", nowUnix, good)),
		"no timestamp":     with("v1=" + good),
		"bad timestamp":    with("t=soon,v1=" + good),
		"zero timestamp":   with("t=0,v1=" + good),
		"not hex":          with(fmt.Sprintf("t=%d,v1=zz%s", nowUnix, good)),
		"truncated sig":    with(fmt.Sprintf("t=%d,v1=%s", nowUnix, good[:20])),
		"uppercase secret": hook(body, strings.ToUpper(testSecret), nowUnix),
	} {
		if err := c.VerifyWebhook(bg, req); err != connector.ErrInvalidSignature {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Within the tolerance either way, and a rolled secret (two v1) both work.
	for _, ts := range []int64{nowUnix - 299, nowUnix + 299} {
		if err := c.VerifyWebhook(bg, hook(body, testSecret, ts)); err != nil {
			t.Errorf("t = now%+d: %v", ts-nowUnix, err)
		}
	}
	rolled := with(fmt.Sprintf("t=%d,v1=%s,v1=%s", nowUnix, sign("whsec_old", nowUnix, body), good))
	if err := c.VerifyWebhook(bg, rolled); err != nil {
		t.Errorf("rolled secret: %v", err)
	}
	// Without ReceivedAt the connector's own clock decides.
	r := hook(body, testSecret, nowUnix-3600)
	r.ReceivedAt = 0
	if err := c.VerifyWebhook(bg, r); err != connector.ErrInvalidSignature {
		t.Errorf("stale by the connector clock: %v", err)
	}
}

func TestParseWebhookEvents(t *testing.T) {
	c := newConn(t, newFake())
	parse := func(name string) []connector.Notification {
		t.Helper()
		ns, err := c.ParseWebhook(bg, hook(fixture(t, name), testSecret, nowUnix))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return ns
	}

	n := parse("event_checkout_session_completed")[0]
	p := n.Payment
	if n.Kind != connector.NotifyPayment || n.EventID != "evt_1" || n.EventType != "checkout.session.completed" || n.OccurredAt != 1760000000000 ||
		p.Status != connector.PaymentSucceeded || p.ProviderRef != "pay_ref_01" || p.ProviderOrderID != "cs_test_a1" || p.ProviderPaymentID != "pi_3A1" ||
		p.Amount != 49900 || p.Currency != "INR" {
		t.Errorf("completed = %+v / %+v", n, p)
	}
	if p := parse("event_checkout_session_expired")[0].Payment; p.Status != connector.PaymentExpired {
		t.Errorf("expired = %+v", p)
	}
	if p := parse("event_checkout_session_async_failed")[0].Payment; p.Status != connector.PaymentFailed || p.ProviderRef != "pay_ref_01" {
		t.Errorf("async failed = %+v", p)
	}

	// A subscription checkout links the subscription to our reference; the payment arrives with its invoice.
	n = parse("event_checkout_session_subscription_completed")[0]
	if n.Kind != connector.NotifySubscription || n.Payment != nil || n.Subscription.ProviderSubscriptionID != "sub_1N1" || n.Subscription.ProviderRef != "SUBREF01" ||
		n.Subscription.Status != "active" {
		t.Errorf("subscription checkout = %+v / %+v", n, n.Subscription)
	}

	n = parse("event_refund_updated")[0]
	if r := n.Refund; n.Kind != connector.NotifyRefund || r.Status != connector.RefundSucceeded || r.ProviderRef != "ref_01" || r.ProviderRefundID != "re_3A1" ||
		r.ProviderPaymentID != "pi_3A1" || r.Amount != 10000 || r.Currency != "INR" {
		t.Errorf("refund = %+v", n.Refund)
	}

	n = parse("event_dispute_created")[0]
	if d := n.Dispute; n.Kind != connector.NotifyDispute || d.ProviderDisputeID != "dp_1" || d.ProviderPaymentID != "pi_3A1" || d.Status != connector.DisputeNeedsResponse ||
		d.Reason != "fraudulent" || d.Amount != 49900 || d.DueBy != (1760000000+7*86400)*1000 || d.Phase != "chargeback" {
		t.Errorf("dispute = %+v", n.Dispute)
	}

	// A renewal: the subscription and the payment together.
	n = parse("event_invoice_paid")[0]
	s, pay := n.Subscription, n.Payment
	if n.Kind != connector.NotifySubscription || s.ProviderSubscriptionID != "sub_1N1" || s.ProviderRef != "SUBREF01" || s.Status != "active" ||
		s.CurrentPeriodEnd != (1760000000+2592000)*1000 || pay == nil || pay.Status != connector.PaymentSucceeded || pay.ProviderPaymentID != "pi_3R2" ||
		pay.SubscriptionID != "sub_1N1" || pay.Amount != 49900 || pay.Currency != "INR" {
		t.Errorf("invoice.paid = %+v / %+v / %+v", n, s, pay)
	}
	// The free first invoice of a trial has no payment.
	if n := parse("event_invoice_paid_free")[0]; n.Kind != connector.NotifySubscription || n.Payment != nil || n.Subscription.ProviderRef != "SUBREF01" {
		t.Errorf("free invoice = %+v", n)
	}
	n = parse("event_invoice_payment_failed")[0]
	if n.Subscription.Status != "past_due" || n.Payment == nil || n.Payment.Status != connector.PaymentFailed || n.Payment.Amount != 49900 {
		t.Errorf("payment failed = %+v / %+v", n.Subscription, n.Payment)
	}
	if s := parse("event_subscription_updated")[0].Subscription; s.Status != "past_due" || s.RawStatus != "past_due" || s.ProviderRef != "SUBREF01" || s.Amount != 49900 {
		t.Errorf("subscription updated = %+v", s)
	}
	if s := parse("event_subscription_deleted")[0].Subscription; s.Status != "canceled" {
		t.Errorf("subscription deleted = %+v", s)
	}
	// Events Paymux does not use yield nothing; a body that is not an event is an error.
	if ns := parse("event_customer_created"); len(ns) != 0 {
		t.Errorf("customer.created = %+v", ns)
	}
	for _, bad := range []string{`{`, `{}`, `{"id":"evt","type":""}`} {
		if _, err := c.ParseWebhook(bg, connector.WebhookRequest{Body: []byte(bad)}); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	_ = time.Second
}

func TestSubscriptionStatusMapping(t *testing.T) {
	for raw, want := range map[string]string{"active": "active", "trialing": "active", "past_due": "past_due", "unpaid": "past_due", "paused": "past_due",
		"canceled": "canceled", "incomplete_expired": "canceled", "incomplete": "incomplete", "some_new_status": "active"} {
		if got := mapSubStatus(raw); got != want {
			t.Errorf("%s -> %s, want %s", raw, got, want)
		}
	}
	for raw, want := range map[string]connector.DisputeStatus{"needs_response": connector.DisputeNeedsResponse, "warning_needs_response": connector.DisputeNeedsResponse,
		"under_review": connector.DisputeUnderReview, "warning_under_review": connector.DisputeUnderReview, "won": connector.DisputeWon, "lost": connector.DisputeLost,
		"warning_closed": connector.DisputeClosed, "charge_refunded": connector.DisputeUnderReview} {
		if got := mapDisputeStatus(raw); got != want {
			t.Errorf("dispute %s -> %s, want %s", raw, got, want)
		}
	}
	for raw, want := range map[string]connector.RefundStatus{"succeeded": connector.RefundSucceeded, "failed": connector.RefundFailed, "canceled": connector.RefundFailed,
		"pending": connector.RefundPending, "requires_action": connector.RefundPending, "new": connector.RefundPending} {
		if got := mapRefundStatus(raw); got != want {
			t.Errorf("refund %s -> %s, want %s", raw, got, want)
		}
	}
}
