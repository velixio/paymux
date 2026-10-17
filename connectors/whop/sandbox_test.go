//go:build sandbox

package whop

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/whop -run Sandbox
// Needs WHOP_TEST_API_KEY, WHOP_TEST_COMPANY_ID and WHOP_TEST_WEBHOOK_SECRET.
func TestSandboxCreateCheckout(t *testing.T) {
	cfg := config{APIKey: os.Getenv("WHOP_TEST_API_KEY"), CompanyID: os.Getenv("WHOP_TEST_COMPANY_ID"), WebhookSecret: os.Getenv("WHOP_TEST_WEBHOOK_SECRET")}
	if cfg.APIKey == "" || cfg.CompanyID == "" || cfg.WebhookSecret == "" {
		t.Skip("WHOP_TEST_* not set")
	}
	raw, _ := json.Marshal(cfg)
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{cfg.APIKey, cfg.WebhookSecret}})
	c, err := New(raw, deps)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: "sbx_" + time.Now().Format("150405"), Amount: 500, Currency: "USD",
	})
	if err != nil || res.ProviderOrderID == "" || res.NextAction.URL == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
