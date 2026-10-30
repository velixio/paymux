//go:build sandbox

package cashfree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/cashfree -run Sandbox -v
// Needs CASHFREE_TEST_CLIENT_ID and CASHFREE_TEST_CLIENT_SECRET (sandbox
// credentials; the sandbox host is always used). Skipped when they are not set.
// Orders are created and a test UPI payment (testsuccess@gocash, see Cashfree's
// sandbox page) is paid through the API; no real money moves. Keys are read
// from the environment only.
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

func sandboxRef(prefix string) string {
	return prefix + strconv.FormatInt(time.Now().UnixNano()/1000, 36)
}

func sandboxCheckout(t *testing.T, c *cashfree, ref string, amount int64) connector.CheckoutResult {
	t.Helper()
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "pay_sbx", AttemptID: "att_sbx", AppID: "app_sbx", ProviderRef: ref,
		Amount: amount, Currency: "INR", Description: "Paymux sandbox check", Customer: connector.Customer{Email: "sbx@example.com", Phone: "9876543210"}})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	return res
}

func TestSandboxOrderIsMatchedStrictly(t *testing.T) {
	c := sandboxConnector(t)
	ctx := context.Background()
	ref := sandboxRef("sbx")
	res := sandboxCheckout(t, c, ref, 10050)
	if res.ProviderOrderID != ref || res.NextAction.Kind != connector.ActionSDK || res.NextAction.Options["payment_session_id"] == "" || res.NextAction.Options["mode"] != "sandbox" {
		t.Fatalf("%+v", res)
	}
	// The same request again (same idempotency key) returns the same order.
	if again := sandboxCheckout(t, c, ref, 10050); again.ProviderOrderID != ref {
		t.Errorf("retry: %+v", again)
	}
	snap, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: ref})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 10050 || snap.ProviderRef != ref || snap.Currency != "INR" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	// The order under another reference must not match.
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: ref, ProviderOrderID: "someone_else"}); err == nil {
		t.Error("an order matched another reference")
	}
}

func TestSandboxUnknownOrderIsNotFound(t *testing.T) {
	c := sandboxConnector(t)
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: sandboxRef("nope")}); !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSandboxBadCredentials(t *testing.T) {
	c := sandboxConnector(t)
	c.api.secret = "wrong_secret_value"
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "whatever"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
}

func TestSandboxExistingOrderIsAdoptedOnlyWhenItMatches(t *testing.T) {
	c := sandboxConnector(t)
	ref := sandboxRef("adopt")
	sandboxCheckout(t, c, ref, 5000)
	// Same order id, different amount: the idempotency key no longer matches (422)
	// or the order exists (409); either way it must not be adopted.
	_, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "p", AttemptID: "a", AppID: "x", ProviderRef: ref, Amount: 6000, Currency: "INR",
		Customer: connector.Customer{Phone: "9876543210"}})
	if err == nil {
		t.Error("an order with another amount was adopted")
	}
}

// payWithTestUPI pays the order through the API with the sandbox's success VPA.
func payWithTestUPI(t *testing.T, c *cashfree, ref string) {
	t.Helper()
	var o orderWire
	if err := c.api.do(context.Background(), "GET", "/orders/"+ref, nil, &o, ""); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"payment_session_id": o.PaymentSessionID, "payment_method": map[string]any{"upi": map[string]any{"channel": "collect", "upi_id": "testsuccess@gocash"}}})
	req, _ := http.NewRequest("POST", sandboxBase+"/orders/sessions", bytes.NewReader(body))
	req.Header.Set("x-client-id", c.api.id)
	req.Header.Set("x-client-secret", c.api.secret)
	req.Header.Set("x-api-version", apiVersion)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pay: HTTP %d", resp.StatusCode)
	}
}

func TestSandboxPaidOrderRefundAndSettlements(t *testing.T) {
	c := sandboxConnector(t)
	ctx := context.Background()
	ref := sandboxRef("paid")
	sandboxCheckout(t, c, ref, 10000)
	payWithTestUPI(t, c, ref)

	var snap connector.PaymentSnapshot
	for i := 0; i < 30; i++ { // the simulated collect completes within about 30 seconds
		var err error
		if snap, err = c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: ref}); err != nil {
			t.Fatal(err)
		}
		if snap.Status == connector.PaymentSucceeded {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if snap.Status != connector.PaymentSucceeded || snap.Amount != 10000 || snap.ProviderPaymentID == "" || snap.Method != "upi" {
		t.Fatalf("the test payment did not succeed: %+v", snap)
	}
	byID, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: snap.ProviderPaymentID, ProviderRef: ref})
	if err != nil || byID.Status != connector.PaymentSucceeded {
		t.Fatalf("by composite id: %+v %v", byID, err)
	}

	rref := "rf" + ref
	if len(rref) > maxRefundID {
		rref = rref[:maxRefundID]
	}
	rq := connector.RefundRequest{ProviderPaymentID: snap.ProviderPaymentID, ProviderRef: rref, Amount: 1025, Currency: "INR", Reason: "sandbox test"}
	r, err := c.Refund(ctx, rq)
	if err != nil || r.ProviderRef != rref || r.Amount != 1025 {
		t.Fatalf("refund: %+v %v", r, err)
	}
	if again, err := c.Refund(ctx, rq); err != nil || again.ProviderRef != rref {
		t.Errorf("a retried refund must return the same refund: %+v %v", again, err)
	}
	got, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRef: rref, ProviderPaymentID: snap.ProviderPaymentID})
	if err != nil || got.Amount != 1025 || got.Status == "" {
		t.Fatalf("FetchRefund: %+v %v", got, err)
	}
	rq.ProviderRef = rref[:len(rref)-1] + "z"
	rq.Amount = 99999999
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(ctx, rq); !errors.As(err, &nr) {
		t.Errorf("refunding more than the payment: %v", err)
	}
	if _, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRef: "no_such_refund", ProviderPaymentID: snap.ProviderPaymentID}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown refund: %v", err)
	}

	now := time.Now()
	if _, err := c.ListSettlements(ctx, connector.TimeRange{From: now.AddDate(0, 0, -60).UnixMilli(), To: now.UnixMilli()}); err != nil {
		t.Fatalf("settlements (the sandbox account has none; the request must still be accepted): %v", err)
	}
}
