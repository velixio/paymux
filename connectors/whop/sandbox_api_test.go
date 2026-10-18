//go:build sandbox

package whop

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

func sandboxWhop(t *testing.T, key string) *whop {
	t.Helper()
	cfg := config{APIKey: key, CompanyID: os.Getenv("WHOP_TEST_COMPANY_ID"), WebhookSecret: "ws_unused", Sandbox: os.Getenv("WHOP_TEST_PRODUCTION") == ""}
	if f := os.Getenv("WHOP_TEST_CONFIG_FILE"); f != "" {
		// A whole account config (api_key, company_id, webhook_secret, sandbox) in a file.
		if b, err := os.ReadFile(f); err == nil {
			var fromFile config
			if json.Unmarshal(b, &fromFile) == nil {
				cfg = fromFile
				if key != "" && key != os.Getenv("WHOP_TEST_API_KEY") {
					cfg.APIKey = key // a test that deliberately uses another key
				}
				key = cfg.APIKey
			}
		}
	} else if os.Getenv("WHOP_TEST_API_KEY") == "" || cfg.CompanyID == "" {
		t.Skip("WHOP_TEST_* not set")
	}
	raw, _ := json.Marshal(cfg)
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{key}})
	c, err := New(raw, deps)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*whop)
}

// The checkout configuration created for a payment must be findable again:
// before the customer pays it reads as in flight, never succeeded or failed.
func TestSandboxCheckoutThenFetch(t *testing.T) {
	w := sandboxWhop(t, os.Getenv("WHOP_TEST_API_KEY"))
	ref := "SBX" + time.Now().Format("150405")
	res, err := w.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref, Amount: 1000, Currency: "USD",
		Description: "Paymux sandbox", Customer: connector.Customer{Email: "sandbox@example.com"},
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	t.Logf("checkout %s url %s", res.ProviderOrderID, res.NextAction.URL)
	snap, err := w.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: res.ProviderOrderID, ProviderRef: ref})
	if err != nil && !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("FetchPayment: %v", err)
	}
	if err == nil && (snap.Status == connector.PaymentSucceeded || snap.Status == connector.PaymentFailed) {
		t.Fatalf("unpaid checkout reported as %s (raw %q)", snap.Status, snap.RawStatus)
	}
	t.Logf("fetch: status=%s raw=%q err=%v", snap.Status, snap.RawStatus, err)
}

func TestSandboxNotFoundMapping(t *testing.T) {
	w := sandboxWhop(t, os.Getenv("WHOP_TEST_API_KEY"))
	_, err := w.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_DoesNotExist0000"})
	if !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("FetchPayment unknown: got %v, want ErrNotFound", err)
	}
}

func TestSandboxInvalidCredentials(t *testing.T) {
	w := sandboxWhop(t, "apik_definitely_not_valid")
	_, err := w.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "p", AttemptID: "a", AppID: "x", ProviderRef: "SBXBAD", Amount: 1000, Currency: "USD"})
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("got %v, want ErrInvalidCredentials", err)
	}
}

func TestSandboxMembershipCheckout(t *testing.T) {
	w := sandboxWhop(t, os.Getenv("WHOP_TEST_API_KEY"))
	res, err := w.CreateSubscriptionCheckout(context.Background(), connector.SubscriptionRequest{
		SubscriptionID: "sub_sbx", ProviderRef: "SBXS" + time.Now().Format("150405"), Amount: 999, Currency: "USD",
		Interval: "month", IntervalCount: 1, Description: "Paymux sandbox membership",
		Customer: connector.Customer{Email: "sandbox@example.com"},
	})
	if err != nil {
		t.Fatalf("CreateSubscriptionCheckout: %v", err)
	}
	t.Logf("membership checkout %s url %s", res.ProviderOrderID, res.NextAction.URL)
}
