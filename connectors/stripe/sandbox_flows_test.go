//go:build sandbox

package stripe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Real events from /v1/events are what Stripe delivers to a webhook endpoint
// (same object). Each is signed here with the connector's test secret, then
// verified and parsed: none may fail, and the money events must be recognised.
func TestSandboxRealEventsVerifyAndParse(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	var list struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := s.api.do(ctx, "GET", "/v1/events", url.Values{"limit": {"100"}}, nil, &list, ""); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) == 0 {
		t.Skip("the account has no events yet")
	}
	ts := time.Now().Unix()
	seen := map[string]int{}
	for _, raw := range list.Data {
		var head struct{ Type string }
		_ = json.Unmarshal(raw, &head)
		req := connector.WebhookRequest{Method: "POST", Body: raw, ReceivedAt: ts * 1000,
			Headers: map[string][]string{"Stripe-Signature": {fmt.Sprintf("t=%d,v1=%s", ts, sign("whsec_unused", ts, raw))}}}
		if err := s.VerifyWebhook(ctx, req); err != nil {
			t.Errorf("%s: verify: %v", head.Type, err)
			continue
		}
		ns, err := s.ParseWebhook(ctx, req)
		if err != nil {
			t.Errorf("%s: parse: %v", head.Type, err)
			continue
		}
		if err := connectortest.CheckNotifications(ns); err != nil {
			t.Errorf("%s: %v", head.Type, err)
		}
		if seen[fmt.Sprintf("%s -> %d", head.Type, len(ns))] == 0 {
			for _, n := range ns {
				b, _ := json.Marshal(n)
				t.Logf("%s: %s", head.Type, b)
			}
		}
		seen[fmt.Sprintf("%s -> %d", head.Type, len(ns))]++
	}
	for k, n := range seen {
		t.Logf("%3d  %s", n, k)
	}
}

// More sandbox flows; same environment as sandbox_test.go. Payments are made
// through the API with Stripe's test payment methods (pm_card_visa,
// pm_card_createDispute); no browser is needed for those. A completed Checkout
// Session needs a browser, so TestSandboxPaidCheckoutSession reads one from
// STRIPE_TEST_PAID_SESSION / STRIPE_TEST_PAID_REF and skips without them.

func sbxRef(prefix string) string { return prefix + strconv.FormatInt(time.Now().UnixNano()/1000, 36) }

// directPayment makes a confirmed PaymentIntent with a test payment method.
func directPayment(t *testing.T, s *stripe, ref, method, currency string, amount int64) string {
	t.Helper()
	f := url.Values{}
	f.Set("amount", strconv.FormatInt(amount, 10))
	f.Set("currency", currency)
	f.Set("payment_method", method)
	f.Set("confirm", "true")
	f.Set("automatic_payment_methods[enabled]", "true")
	f.Set("automatic_payment_methods[allow_redirects]", "never")
	f.Set("metadata["+metaRef+"]", ref)
	var pi struct {
		ID, Status string
	}
	if err := s.api.do(context.Background(), "POST", "/v1/payment_intents", nil, f, &pi, "pi:"+ref); err != nil {
		t.Fatal(err)
	}
	if pi.Status != "succeeded" {
		t.Fatalf("payment intent %s is %s", pi.ID, pi.Status)
	}
	return pi.ID
}

func TestSandboxUnknownAndBadCredentials(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	if _, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: "cs_test_doesnotexist", ProviderRef: "x"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if _, err := s.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: "re_doesnotexist"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown refund: %v", err)
	}
	if _, err := s.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: "dp_doesnotexist"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown dispute: %v", err)
	}
	s.api.key = "bad_key_value"
	if _, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: "cs_test_x", ProviderRef: "x"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("bad key: %v", err)
	}
}

func TestSandboxCheckoutRetryIsIdempotent(t *testing.T) {
	s := sandboxConnector(t)
	ref := sbxRef("idem")
	req := connector.CheckoutRequest{PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref, Amount: 49900, Currency: "INR", Description: "Paymux sandbox"}
	a, err := s.CreateCheckout(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateCheckout(context.Background(), req)
	if err != nil || a.ProviderOrderID != b.ProviderOrderID {
		t.Fatalf("a retry must return the same session: %q %q %v", a.ProviderOrderID, b.ProviderOrderID, err)
	}
}

// Amounts per currency class: two-decimal (INR, USD), zero-decimal (JPY)
// and HUF (two-decimal in the API but a whole number of forints), through the real Checkout Session.
func TestSandboxCurrencies(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	for _, tc := range []struct {
		cur    string
		amount int64 // minor units as Paymux holds them
	}{{"INR", 49900}, {"USD", 2550}, {"JPY", 500}, {"EUR", 100}, {"HUF", 150000}} {
		ref := sbxRef("cur" + tc.cur)
		res, err := s.CreateCheckout(ctx, connector.CheckoutRequest{PaymentID: "p", AttemptID: "a", AppID: "x", ProviderRef: ref, Amount: tc.amount, Currency: tc.cur, Description: "currency check"})
		if err != nil {
			t.Errorf("%s: %v", tc.cur, err)
			continue
		}
		snap, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: res.ProviderOrderID, ProviderRef: ref})
		if err != nil || snap.Amount != tc.amount || snap.Currency != tc.cur {
			t.Errorf("%s: %+v %v (want %d)", tc.cur, snap, err, tc.amount)
		}
	}
}

func TestSandboxPaidCheckoutSession(t *testing.T) {
	s := sandboxConnector(t)
	session, ref := os.Getenv("STRIPE_TEST_PAID_SESSION"), os.Getenv("STRIPE_TEST_PAID_REF")
	if session == "" || ref == "" {
		t.Skip("STRIPE_TEST_PAID_SESSION / STRIPE_TEST_PAID_REF not set (a Checkout Session paid in a browser)")
	}
	ctx := context.Background()
	snap, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: session, ProviderRef: ref})
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderPaymentID == "" || snap.Method != "card" {
		t.Fatalf("%+v %v", snap, err)
	}
	if _, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: session, ProviderRef: "someone_else"}); err == nil {
		t.Error("a paid session matched another reference")
	}
	refundFlow(t, s, snap.ProviderPaymentID, snap.Currency, snap.Amount)
}

func refundFlow(t *testing.T, s *stripe, pi, cur string, total int64) {
	t.Helper()
	ctx := context.Background()
	rref := sbxRef("rf")
	rq := connector.RefundRequest{ProviderPaymentID: pi, ProviderRef: rref, Amount: total / 4, Currency: cur, Reason: "requested_by_customer"}
	r, err := s.Refund(ctx, rq)
	if err != nil || r.ProviderRef != rref || r.Amount != rq.Amount || r.Status != connector.RefundSucceeded {
		t.Fatalf("refund: %+v %v", r, err)
	}
	again, err := s.Refund(ctx, rq)
	if err != nil || again.ProviderRefundID != r.ProviderRefundID {
		t.Errorf("a retried refund (same Idempotency-Key) must return the same refund: %+v %v", again, err)
	}
	if got, err := s.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: r.ProviderRefundID}); err != nil || got.Amount != rq.Amount || got.ProviderRef != rref {
		t.Errorf("FetchRefund by id: %+v %v", got, err)
	}
	if got, err := s.FetchRefund(ctx, connector.RefundRef{ProviderRef: rref, ProviderPaymentID: pi}); err != nil || got.ProviderRefundID != r.ProviderRefundID {
		t.Errorf("FetchRefund by ref: %+v %v", got, err)
	}
	over := connector.RefundRequest{ProviderPaymentID: pi, ProviderRef: sbxRef("over"), Amount: total * 10, Currency: cur}
	var nr connector.ErrNotRefundable
	if _, err := s.Refund(ctx, over); !errors.As(err, &nr) {
		t.Errorf("over-refund: %v", err)
	}
}

func TestSandboxDirectPaymentRefundAndFees(t *testing.T) {
	s := sandboxConnector(t)
	ref := sbxRef("direct")
	pi := directPayment(t, s, ref, "pm_card_visa", "usd", 4000)
	refundFlow(t, s, pi, "USD", 4000)
	now := time.Now()
	tr := connector.TimeRange{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(time.Hour).UnixMilli()}
	page, err := s.ListPayments(context.Background(), tr, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range page.Items {
		if p.ProviderPaymentID == pi {
			found = p.ProviderRef == ref && p.Amount == 4000 && p.Status == connector.PaymentSucceeded
		}
	}
	if !found {
		t.Errorf("ListPayments did not return %s with our ref (%d items)", pi, len(page.Items))
	}
}

func TestSandboxDisputeLifecycle(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 2; i++ {
		pi := directPayment(t, s, sbxRef("disp"), "pm_card_createDispute", "usd", 2500)
		var dispute connector.DisputeSnapshot
		for try := 0; try < 20 && dispute.ProviderDisputeID == ""; try++ {
			time.Sleep(2 * time.Second)
			now := time.Now()
			page, err := s.ListDisputes(ctx, connector.TimeRange{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(time.Hour).UnixMilli()}, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range page.Items {
				if d.ProviderPaymentID == pi {
					dispute = d
				}
			}
		}
		if dispute.ProviderDisputeID == "" {
			t.Fatalf("no dispute appeared for %s", pi)
		}
		if dispute.Amount != 2500 || dispute.Currency != "USD" || dispute.Status == "" {
			t.Errorf("%+v", dispute)
		}
		ids = append(ids, dispute.ProviderDisputeID)
	}
	got, err := s.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: ids[0]})
	if err != nil || got.ProviderDisputeID != ids[0] || got.DueBy == 0 {
		t.Fatalf("FetchDispute: %+v %v", got, err)
	}
	if err := s.SubmitEvidence(ctx, connector.DisputeRef{ProviderDisputeID: ids[0]}, connector.Evidence{Text: "Delivered as ordered. Sandbox evidence.", Files: map[string][]byte{"receipt": tinyPNG}}); err != nil {
		t.Errorf("SubmitEvidence: %v", err)
	}
	if err := s.AcceptDispute(ctx, connector.DisputeRef{ProviderDisputeID: ids[1]}); err != nil {
		t.Errorf("AcceptDispute: %v", err)
	}
	after, err := s.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: ids[1]})
	if err != nil || after.Status == got.Status && after.RawStatus == got.RawStatus {
		t.Logf("dispute after accept: %+v %v", after, err)
	}
}

func TestSandboxSettlements(t *testing.T) {
	s := sandboxConnector(t)
	now := time.Now()
	got, err := s.ListSettlements(context.Background(), connector.TimeRange{From: now.AddDate(0, 0, -90).UnixMilli(), To: now.AddDate(0, 0, 30).UnixMilli()})
	if err != nil {
		t.Fatalf("ListSettlements: %v", err)
	}
	t.Logf("%d payouts in the window", len(got))
	for _, p := range got {
		t.Logf("%+v", p)
	}
}

// A subscription made straight through the API (a customer with a test card and
// an inline price on a product), then read, repriced and cancelled through the
// connector.
func TestSandboxSubscriptionLifecycle(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	var cust, prod struct{ ID string }
	if err := s.api.do(ctx, "POST", "/v1/customers", nil, url.Values{"email": {"sbx-sub@example.com"}, "payment_method": {"pm_card_visa"}, "invoice_settings[default_payment_method]": {"pm_card_visa"}}, &cust, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.api.do(ctx, "POST", "/v1/products", nil, url.Values{"name": {"Paymux sandbox plan"}}, &prod, ""); err != nil {
		t.Fatal(err)
	}
	ref := sbxRef("sub")
	f := url.Values{}
	f.Set("customer", cust.ID)
	f.Set("items[0][price_data][currency]", "usd")
	f.Set("items[0][price_data][product]", prod.ID)
	f.Set("items[0][price_data][unit_amount]", "1000")
	f.Set("items[0][price_data][recurring][interval]", "month")
	f.Set("metadata["+metaRef+"]", ref)
	var sub struct{ ID string }
	if err := s.api.do(ctx, "POST", "/v1/subscriptions", nil, f, &sub, ""); err != nil {
		t.Fatal(err)
	}
	r := connector.SubscriptionRef{ProviderSubscriptionID: sub.ID}
	snap, err := s.FetchSubscription(ctx, r)
	if err != nil || snap.Amount != 1000 || snap.Currency != "USD" || snap.ProviderRef != ref || snap.CurrentPeriodEnd == 0 {
		t.Fatalf("FetchSubscription: %+v %v", snap, err)
	}
	if err := s.UpdateSubscriptionAmount(ctx, r, 1500); err != nil {
		t.Fatalf("UpdateSubscriptionAmount: %v", err)
	}
	if snap, err = s.FetchSubscription(ctx, r); err != nil || snap.Amount != 1500 {
		t.Fatalf("after the update: %+v %v", snap, err)
	}
	if err := s.UpdateSubscriptionAmount(ctx, r, 1500); err != nil {
		t.Errorf("repeating the same update: %v", err)
	}
	if err := s.CancelSubscription(ctx, r, true); err != nil {
		t.Fatalf("cancel at period end: %v", err)
	}
	if err := s.CancelSubscription(ctx, r, false); err != nil {
		t.Fatalf("cancel now: %v", err)
	}
	if snap, err = s.FetchSubscription(ctx, r); err != nil || snap.Status == "active" {
		t.Errorf("after cancel: %+v %v", snap, err)
	}
	if _, err := s.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: "sub_doesnotexist"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown subscription: %v", err)
	}
}

// tinyPNG is a valid 1x1 PNG: the Files API checks that an upload really is an image.
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
