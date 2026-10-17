//go:build sandbox

package stripe

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/stripe -run Sandbox
// Needs STRIPE_TEST_SECRET_KEY (an sk_test_ or rk_test_ key). Nothing is paid:
// the tests create a Checkout Session, read it back and list, which test mode
// allows freely. Skipped when the key is not set.
func sandboxConnector(t *testing.T) *stripe {
	t.Helper()
	key := os.Getenv("STRIPE_TEST_SECRET_KEY")
	if key == "" {
		t.Skip("STRIPE_TEST_SECRET_KEY not set")
	}
	if len(key) < 8 || key[:8] != "sk_test_" && key[:8] != "rk_test_" {
		t.Fatal("STRIPE_TEST_SECRET_KEY must be a test-mode key")
	}
	cfg, _ := json.Marshal(config{SecretKey: key, WebhookSecret: "whsec_unused"})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{key}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*stripe)
}

func TestSandboxCheckoutSessionIsMatchedStrictly(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	ref := "sbx_" + time.Now().Format("150405")
	res, err := s.CreateCheckout(ctx, connector.CheckoutRequest{PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref,
		Amount: 5000, Currency: "USD", Description: "Paymux sandbox check"})
	if err != nil || res.ProviderOrderID == "" || res.NextAction.URL == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	snap, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: res.ProviderOrderID, ProviderRef: ref})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 5000 || snap.Currency != "USD" || snap.ProviderRef != ref {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	// The same session under another reference must not match.
	if _, err := s.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: res.ProviderOrderID, ProviderRef: "someone_else"}); err == nil {
		t.Error("a session matched another reference")
	}
}

func TestSandboxListingsParse(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	tr := connector.TimeRange{From: time.Now().Add(-30 * 24 * time.Hour).UnixMilli(), To: time.Now().UnixMilli()}
	if _, err := s.ListPayments(ctx, tr, ""); err != nil {
		t.Errorf("ListPayments: %v", err)
	}
	if _, err := s.ListRefunds(ctx, tr, ""); err != nil {
		t.Errorf("ListRefunds: %v", err)
	}
	if _, err := s.ListDisputes(ctx, tr, ""); err != nil {
		t.Errorf("ListDisputes: %v", err)
	}
	if _, err := s.ListSettlements(ctx, tr); err != nil {
		t.Errorf("ListSettlements: %v", err)
	}
}

func TestSandboxSubscriptionCheckout(t *testing.T) {
	s := sandboxConnector(t)
	ref := "sbxsub_" + time.Now().Format("150405")
	res, err := s.CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{SubscriptionID: "sub_sbx", ProviderRef: ref, Amount: 1000,
		Currency: "USD", Interval: "month", IntervalCount: 1, Description: "Paymux sandbox plan"})
	if err != nil || res.ProviderOrderID == "" || res.NextAction.URL == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
