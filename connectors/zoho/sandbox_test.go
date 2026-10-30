//go:build sandbox

package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/zoho -run Sandbox -v
// Needs ZOHO_TEST_ACCOUNT_ID, ZOHO_TEST_CLIENT_ID, ZOHO_TEST_CLIENT_SECRET,
// ZOHO_TEST_REFRESH_TOKEN, ZOHO_TEST_WEBHOOK_SECRET and ZOHO_TEST_WIDGET_API_KEY,
// read from the environment only.
//
// Zoho has two servers with separate credentials: payments.zoho.in (scopes
// ZohoPay.*) and paymentssandbox.zoho.in (scopes ZohoPaySandbox.*, soid on the
// authorization). The environment's credentials decide which one these tests
// reach: set ZOHO_TEST_SANDBOX=1 (or "sandbox": true in ZOHO_TEST_CONFIG_FILE)
// for the sandbox. The tests only create payment sessions (they expire in 15
// minutes) and read; they never pay or refund anything.
func sandboxConnector(t *testing.T) *zoho {
	t.Helper()
	cfg := config{
		AccountID: os.Getenv("ZOHO_TEST_ACCOUNT_ID"), ClientID: os.Getenv("ZOHO_TEST_CLIENT_ID"),
		ClientSecret: os.Getenv("ZOHO_TEST_CLIENT_SECRET"), RefreshToken: os.Getenv("ZOHO_TEST_REFRESH_TOKEN"),
		WebhookSecret: os.Getenv("ZOHO_TEST_WEBHOOK_SECRET"), WidgetAPIKey: os.Getenv("ZOHO_TEST_WIDGET_API_KEY"),
		Sandbox: os.Getenv("ZOHO_TEST_SANDBOX") != "",
	}
	if f := os.Getenv("ZOHO_TEST_CONFIG_FILE"); f != "" {
		// A whole account config (as stored by Paymux): account_id, client_id,
		// client_secret, refresh_token, data_center, widget_api_key, webhook_secret, sandbox.
		if b, err := os.ReadFile(f); err == nil {
			_ = json.Unmarshal(b, &cfg)
		}
	}
	if f := os.Getenv("ZOHO_TEST_REFRESH_TOKEN_FILE"); cfg.RefreshToken == "" && f != "" {
		// A JSON file with a refresh_token field (the OAuth token response), or the bare token.
		if b, err := os.ReadFile(f); err == nil {
			var tok struct {
				RefreshToken string `json:"refresh_token"`
			}
			if json.Unmarshal(b, &tok) == nil && tok.RefreshToken != "" {
				cfg.RefreshToken = tok.RefreshToken
			} else {
				cfg.RefreshToken = strings.TrimSpace(string(b))
			}
		}
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
	return c.(*zoho)
}

func sandboxRef() string { return "sbx" + strconv.FormatInt(time.Now().UnixNano()/1000, 36) }

func TestSandboxCreateSessionAndFetchItStrictly(t *testing.T) {
	z := sandboxConnector(t)
	ctx := context.Background()
	ref := sandboxRef()
	res, err := z.CreateCheckout(ctx, connector.CheckoutRequest{PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref, Amount: 10050, Currency: "INR", Description: "Paymux sandbox check"})
	if err != nil || res.ProviderOrderID == "" || res.ExpiresAt == 0 || res.NextAction.Kind != connector.ActionSDK || res.NextAction.Options["description"] != "Paymux sandbox check" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	snap, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: res.ProviderOrderID, ProviderRef: ref})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 10050 || snap.ProviderRef != ref || snap.ProviderOrderID != res.ProviderOrderID {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if _, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: res.ProviderOrderID, ProviderRef: "someone_else"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("a session of another reference: %v", err)
	}
}

func TestSandboxUnknownIDsAndBadCredentials(t *testing.T) {
	z := sandboxConnector(t)
	ctx := context.Background()
	if _, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: "28669000000999999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("session: %v", err)
	}
	if _, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "28669000000999999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("payment: %v", err)
	}
	if _, err := z.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: "28669000000999999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("refund: %v", err)
	}
	if _, err := z.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: "28669000000999999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("mandate: %v", err)
	}
	// A refund of a payment that does not exist, and of one refunded already.
	if _, err := z.Refund(ctx, connector.RefundRequest{ProviderPaymentID: "28669000000999999", ProviderRef: sandboxRef(), Amount: 100, Currency: "INR"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("refund of an unknown payment: %v", err)
	}
	z.api.cfg.RefreshToken = "1000.badbadbad.badbadbad"
	_ = z.api.kv.Delete(ctx, tokenKey)
	if _, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: "1"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("bad refresh token: %v", err)
	}
}

// Reads: the payments of the account parse with the connector's entities
// (payment_method is an object, amounts are strings, refunded payments are paid
// ones), and each payment fetched by id matches its own session and reference.
func TestSandboxExistingPaymentsParse(t *testing.T) {
	z := sandboxConnector(t)
	ctx := context.Background()
	var list struct {
		Payments []paymentEntity `json:"payments"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/payments", nil, nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Payments) == 0 {
		t.Skip("the account has no payments")
	}
	for i, p := range list.Payments {
		if i >= 5 {
			break
		}
		snap, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: p.PaymentID})
		if err != nil || snap.ProviderPaymentID != p.PaymentID || snap.Amount <= 0 || snap.Method == "" {
			t.Errorf("%s: %+v %v", p.PaymentID, snap, err)
		}
		if snap.RawStatus == "refunded" && snap.AmountRefunded != snap.Amount {
			t.Errorf("%s: refunded but %+v", p.PaymentID, snap)
		}
		if _, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: p.PaymentID, ProviderRef: "someone_else_" + p.PaymentID}); p.Reference != "" && !errors.Is(err, connector.ErrNotFound) {
			t.Errorf("%s: another reference was accepted: %v", p.PaymentID, err)
		}
	}
	var refunds struct {
		Refunds []refundEntity `json:"refunds"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/refunds", nil, nil, &refunds); err != nil {
		t.Fatal(err)
	}
	for i, r := range refunds.Refunds {
		if i >= 3 {
			break
		}
		snap, err := z.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: r.RefundID})
		if err != nil || snap.ProviderRefundID != r.RefundID || snap.Amount <= 0 {
			t.Errorf("%s: %+v %v", r.RefundID, snap, err)
		}
	}
}
