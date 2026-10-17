//go:build sandbox

package razorpay

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/razorpay -run Sandbox
// Needs RAZORPAY_TEST_KEY_ID, RAZORPAY_TEST_KEY_SECRET and
// RAZORPAY_TEST_WEBHOOK_SECRET (test-mode keys from the Razorpay dashboard).
func TestSandboxCreateOrder(t *testing.T) {
	id, secret, wh := os.Getenv("RAZORPAY_TEST_KEY_ID"), os.Getenv("RAZORPAY_TEST_KEY_SECRET"), os.Getenv("RAZORPAY_TEST_WEBHOOK_SECRET")
	if id == "" || secret == "" || wh == "" {
		t.Skip("RAZORPAY_TEST_* not set")
	}
	cfg, _ := json.Marshal(config{KeyID: id, KeySecret: secret, WebhookSecret: wh})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{secret, wh}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	ref := "sbx_" + time.Now().Format("150405")
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref, Amount: 100, Currency: "INR",
	})
	if err != nil || res.ProviderOrderID == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: res.ProviderOrderID})
	if err != nil || snap.Status != connector.PaymentRequiresAction {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}
