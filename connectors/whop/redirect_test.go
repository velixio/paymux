package whop

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Whop rejects a non-https redirect_url (sandbox, 2026-10-17): "The redirect
// URL must be a valid URL, starting with https://". A local http base URL must
// not break checkout; the redirect is omitted and the poller/webhook resolve
// the payment instead.
func TestNonHTTPSReturnURLIsOmitted(t *testing.T) {
	f := newFake("")
	srv := httptest.NewServer(f)
	defer srv.Close()
	deps := connectortest.NewDeps(srv, time.Unix(nowUnix, 0))
	deps.URLs = connectortest.URLs{Base: "http://localhost:8080"}
	c, err := New(testConfig, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "p", AttemptID: "a", AppID: "x", ProviderRef: "R1", Amount: 1000, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/checkout_configurations"], &body)
	if _, ok := body["redirect_url"]; ok {
		t.Fatalf("http redirect_url sent: %v", body["redirect_url"])
	}
	if _, err := c.(connector.SubscriptionManager).CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{ProviderRef: "R2", Amount: 1000,
		Currency: "USD", Interval: "month", IntervalCount: 1}); err != nil {
		t.Fatal(err)
	}
	body = nil
	_ = json.Unmarshal(f.bodies["POST /api/v1/checkout_configurations"], &body)
	if _, ok := body["redirect_url"]; ok {
		t.Fatalf("http redirect_url sent for subscription: %v", body["redirect_url"])
	}
}
