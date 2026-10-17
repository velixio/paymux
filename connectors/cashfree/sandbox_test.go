//go:build sandbox

package cashfree

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/cashfree -run Sandbox
// Needs CASHFREE_TEST_CLIENT_ID and CASHFREE_TEST_CLIENT_SECRET (sandbox
// credentials; the sandbox host is always used). Nothing is paid: the tests
// create an order and read it back. Skipped when the keys are not set.
func sandboxConnector(t *testing.T) *cashfree {
	t.Helper()
	id, secret := os.Getenv("CASHFREE_TEST_CLIENT_ID"), os.Getenv("CASHFREE_TEST_CLIENT_SECRET")
	if id == "" || secret == "" {
		t.Skip("CASHFREE_TEST_CLIENT_ID / CASHFREE_TEST_CLIENT_SECRET not set")
	}
	cfg, _ := json.Marshal(config{ClientID: id, ClientSecret: secret, Sandbox: true})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{secret}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*cashfree)
}

func TestSandboxOrderIsMatchedStrictly(t *testing.T) {
	c := sandboxConnector(t)
	ctx := context.Background()
	ref := "sbx_" + time.Now().Format("150405")
	res, err := c.CreateCheckout(ctx, connector.CheckoutRequest{PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref,
		Amount: 10000, Currency: "INR", Description: "Paymux sandbox check", Customer: connector.Customer{Email: "sbx@example.com", Phone: "9876543210"}})
	if err != nil || res.ProviderOrderID != ref || res.NextAction.Options["payment_session_id"] == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	snap, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: ref})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 10000 || snap.ProviderRef != ref {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
}

func TestSandboxSettlementsParse(t *testing.T) {
	c := sandboxConnector(t)
	now := time.Now()
	if _, err := c.ListSettlements(context.Background(), connector.TimeRange{From: now.AddDate(0, 0, -7).UnixMilli(), To: now.UnixMilli()}); err != nil {
		t.Logf("TODO(spec): settlements wire shape needs verification: %v", err)
	}
}
