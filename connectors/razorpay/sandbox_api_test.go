//go:build sandbox

package razorpay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

func sandboxConnector(t *testing.T) *razorpay {
	t.Helper()
	id, secret := os.Getenv("RAZORPAY_TEST_KEY_ID"), os.Getenv("RAZORPAY_TEST_KEY_SECRET")
	if id == "" || secret == "" {
		t.Skip("RAZORPAY_TEST_* not set")
	}
	cfg, _ := json.Marshal(config{KeyID: id, KeySecret: secret, WebhookSecret: "unused"})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{secret}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*razorpay)
}

// The read APIs must parse real responses (empty or not) without error.
func TestSandboxListings(t *testing.T) {
	r := sandboxConnector(t)
	ctx := context.Background()
	now := time.Now()
	tr := connector.TimeRange{From: now.Add(-30 * 24 * time.Hour).UnixMilli(), To: now.UnixMilli()}
	if p, err := r.ListPayments(ctx, tr, ""); err != nil {
		t.Errorf("ListPayments: %v", err)
	} else {
		t.Logf("payments in last 30d: %d", len(p.Items))
	}
	if p, err := r.ListRefunds(ctx, tr, ""); err != nil {
		t.Errorf("ListRefunds: %v", err)
	} else {
		t.Logf("refunds: %d", len(p.Items))
	}
	if p, err := r.ListDisputes(ctx, tr, ""); err != nil {
		t.Errorf("ListDisputes: %v", err)
	} else {
		t.Logf("disputes: %d", len(p.Items))
	}
	if s, err := r.ListSettlements(ctx, tr); err != nil {
		t.Errorf("ListSettlements: %v", err)
	} else {
		t.Logf("settlements: %d", len(s))
	}
}

// Unknown ids must map to ErrNotFound (the dashboard "test connection" relies on it).
func TestSandboxNotFoundMapping(t *testing.T) {
	r := sandboxConnector(t)
	_, err := r.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "order_00000000000000"})
	if !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("FetchPayment unknown order: got %v, want ErrNotFound", err)
	}
	_, err = r.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rfnd_00000000000000"})
	if !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("FetchRefund unknown refund: got %v, want ErrNotFound", err)
	}
}

// Bad credentials must map to ErrInvalidCredentials.
func TestSandboxInvalidCredentials(t *testing.T) {
	if os.Getenv("RAZORPAY_TEST_KEY_ID") == "" {
		t.Skip("RAZORPAY_TEST_* not set")
	}
	cfg, _ := json.Marshal(config{KeyID: os.Getenv("RAZORPAY_TEST_KEY_ID"), KeySecret: "wrong-secret", WebhookSecret: "x"})
	c, err := New(cfg, connectortest.NewDeps(nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "p", AttemptID: "a", AppID: "x", ProviderRef: "sbxbad", Amount: 100, Currency: "INR"})
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("got %v, want ErrInvalidCredentials", err)
	}
}

// Provider-managed subscription: plan + subscription creation must succeed
// (requires Subscriptions to be enabled on the Razorpay test account).
func TestSandboxSubscriptionCreate(t *testing.T) {
	r := sandboxConnector(t)
	res, err := r.CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{
		SubscriptionID: "sub_sbx", ProviderRef: "SBX" + time.Now().Format("150405"), Amount: 49900, Currency: "INR",
		Interval: "month", IntervalCount: 1, TotalCount: 12, Description: "Paymux sandbox plan",
		Customer: connector.Customer{Name: "Sandbox Tester", Email: "sandbox@example.com"},
	})
	if err != nil {
		t.Fatalf("CreateSubscriptionCheckout: %v", err)
	}
	t.Logf("provider subscription %s next action %s", res.ProviderOrderID, res.NextAction.Kind)
	snap, err := r.FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: res.ProviderOrderID})
	if err != nil {
		t.Fatalf("FetchSubscription: %v", err)
	}
	t.Logf("status %s raw %s amount %d", snap.Status, snap.RawStatus, snap.Amount)
	if err := r.CancelSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: res.ProviderOrderID}, false); err != nil {
		t.Errorf("CancelSubscription: %v", err)
	}
}
