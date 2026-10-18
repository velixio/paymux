//go:build sandbox

package payu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/payu -run Sandbox -v
// Needs PAYU_TEST_KEY and PAYU_TEST_SALT (test.payu.in credentials), read from
// the environment only. A payment cannot be made through the API, so the flows
// that need one read it from PAYU_TEST_PAID_TXNID (a txnid paid in a browser; see
// sandbox_browser_test.go) and skip without it.
func sandboxConnector(t *testing.T) *payu {
	t.Helper()
	key, salt := os.Getenv("PAYU_TEST_KEY"), os.Getenv("PAYU_TEST_SALT")
	if key == "" || salt == "" {
		t.Skip("PAYU_TEST_* not set")
	}
	cfg, _ := json.Marshal(config{Key: key, Salt: salt, TestMode: true})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{salt}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*payu)
}

// PayU answers an unknown txnid with 200 and "Not Found": the same as a checkout
// the customer has not submitted yet. Paymux only asks about txnids it issued,
// so this is reported as a payment still waiting for the customer.
func TestSandboxVerifyUnknownTxn(t *testing.T) {
	c := sandboxConnector(t)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "sbx_does_not_exist_" + strconv.FormatInt(time.Now().UnixNano(), 36)})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.ProviderPaymentID != "" {
		t.Fatalf("%+v %v", snap, err)
	}
}

func TestSandboxBadKeyOrSaltIsInvalidCredentials(t *testing.T) {
	c := sandboxConnector(t)
	c.api.salt = "wrong"
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "whatever"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("bad salt: %v", err)
	}
	c = sandboxConnector(t)
	c.api.key = "NOKEY"
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "whatever"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("bad key: %v", err)
	}
}

// A payment paid in a browser: fetched by txnid, then refunded twice with
// distinct tokens, one of them retried, plus an over-refund.
func TestSandboxPaidPaymentAndRefunds(t *testing.T) {
	c := sandboxConnector(t)
	txn := os.Getenv("PAYU_TEST_PAID_TXNID")
	if txn == "" {
		t.Skip("PAYU_TEST_PAID_TXNID not set (a txnid paid in the browser)")
	}
	ctx := context.Background()
	snap, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: txn})
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderPaymentID == "" || snap.Method == "" || snap.Amount <= 0 {
		t.Fatalf("%+v %v", snap, err)
	}
	// PayU refuses two refunds of one amount on one payment within 5 minutes, so the amount varies.
	refundAmount := 100 + time.Now().Unix()%89
	tok := "sbx" + strconv.FormatInt(time.Now().Unix(), 36)
	rq := connector.RefundRequest{ProviderPaymentID: snap.ProviderPaymentID, ProviderRef: tok, Amount: refundAmount, Currency: "INR"}
	r, err := c.Refund(ctx, rq)
	if err != nil || r.ProviderRefundID == "" || r.Status == "" {
		t.Fatalf("refund: %+v %v", r, err)
	}
	again, err := c.Refund(ctx, rq)
	if err != nil || again.ProviderRefundID != r.ProviderRefundID {
		t.Errorf("a retried refund must return the same refund: %+v %v", again, err)
	}
	byReq, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: r.ProviderRefundID})
	if err != nil || byReq.Amount != refundAmount || byReq.ProviderRef != tok {
		t.Errorf("by request id: %+v %v", byReq, err)
	}
	byTok, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRef: tok, ProviderPaymentID: snap.ProviderPaymentID})
	if err != nil || byTok.ProviderRefundID != r.ProviderRefundID {
		t.Errorf("by token: %+v %v", byTok, err)
	}
	over := connector.RefundRequest{ProviderPaymentID: snap.ProviderPaymentID, ProviderRef: tok + "o", Amount: snap.Amount * 100, Currency: "INR"}
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(ctx, over); !errors.As(err, &nr) {
		t.Errorf("over-refund: %v", err)
	}
	if _, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: "900000000999999999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown refund: %v", err)
	}
	now := time.Now()
	pg, err := c.ListPayments(ctx, connector.TimeRange{From: now.Add(-24 * time.Hour).UnixMilli(), To: now.Add(time.Hour).UnixMilli()}, "")
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	found := false
	for _, p := range pg.Items {
		if p.ProviderRef == txn {
			found = p.Status == connector.PaymentSucceeded && p.ProviderPaymentID == snap.ProviderPaymentID && p.Fee > 0
		}
	}
	if !found {
		for _, p := range pg.Items {
			t.Logf("%+v", p)
		}
		t.Errorf("ListPayments did not return %s as succeeded with its fee (%d items)", txn, len(pg.Items))
	}
}

// The standing-instruction commands exist and take the var1 Paymux sends; an
// unknown mandate is ErrNotFound.
func TestSandboxMandateCommandsOnAnUnknownMandate(t *testing.T) {
	c := sandboxConnector(t)
	ctx := context.Background()
	m := connector.MandateRef{ProviderMandateID: "1999999999999"}
	if _, err := c.FetchMandate(ctx, m); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("FetchMandate: %v", err)
	}
	if err := c.RevokeMandate(ctx, m); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("RevokeMandate: %v", err)
	}
}
