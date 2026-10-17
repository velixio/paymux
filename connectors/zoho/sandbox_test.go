//go:build sandbox

package zoho

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/zoho -run Sandbox
// Needs ZOHO_TEST_ACCOUNT_ID, ZOHO_TEST_CLIENT_ID, ZOHO_TEST_CLIENT_SECRET,
// ZOHO_TEST_REFRESH_TOKEN, ZOHO_TEST_WEBHOOK_SECRET and ZOHO_TEST_WIDGET_API_KEY.
// Zoho support must enable the sandbox for the account first.
func TestSandboxCreateSession(t *testing.T) {
	cfg := config{
		AccountID: os.Getenv("ZOHO_TEST_ACCOUNT_ID"), ClientID: os.Getenv("ZOHO_TEST_CLIENT_ID"),
		ClientSecret: os.Getenv("ZOHO_TEST_CLIENT_SECRET"), RefreshToken: os.Getenv("ZOHO_TEST_REFRESH_TOKEN"),
		WebhookSecret: os.Getenv("ZOHO_TEST_WEBHOOK_SECRET"), WidgetAPIKey: os.Getenv("ZOHO_TEST_WIDGET_API_KEY"),
	}
	if cfg.AccountID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RefreshToken == "" || cfg.WebhookSecret == "" || cfg.WidgetAPIKey == "" {
		t.Skip("ZOHO_TEST_* not set")
	}
	raw, _ := json.Marshal(cfg)
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{cfg.ClientSecret, cfg.RefreshToken}})
	c, err := New(raw, deps)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: "sbx_" + time.Now().Format("150405"), Amount: 100, Currency: "INR",
	})
	if err != nil || res.ProviderOrderID == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
