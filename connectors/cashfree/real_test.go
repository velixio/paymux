package cashfree

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

// The fixtures real_*.json are responses recorded from the real Cashfree
// sandbox (api version 2023-08-01), sanitised: ids and tokens are replaced.
// Notable real shapes: cf_* ids are JSON strings, amounts are JSON numbers
// with a fractional part, unset fields are null.

func raw(t testing.TB, name string) json.RawMessage { return json.RawMessage(fixture(t, name)) }

func TestRealShapesDecode(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01", 200, raw(t, "real_order"))
	f.reply("GET /pg/orders/pay_ref_01/payments", 200, raw(t, "real_payments"))
	snap, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.PaymentSucceeded || snap.Amount != 10050 || snap.ProviderPaymentID != "pay_ref_01:1465089236821754368" || snap.Method != "upi" {
		t.Errorf("%+v", snap)
	}
	f.reply("GET /pg/orders/pay_ref_01/refunds/rf_ref_01", 200, raw(t, "real_refund"))
	r, err := newConn(t, f).(connector.Refunder).FetchRefund(context.Background(), connector.RefundRef{ProviderRef: "rf_ref_01", ProviderPaymentID: snap.ProviderPaymentID})
	if err != nil || r.Status != connector.RefundPending || r.Amount != 1025 || r.ProviderPaymentID != snap.ProviderPaymentID {
		t.Errorf("%+v %v", r, err)
	}
}

// Webhook versions from 2025-01-01 send x-idempotency-key, unique per payload.
func TestWebhookEventIDUsesTheIdempotencyKey(t *testing.T) {
	c := newConn(t, newFake())
	req := hook(fixture(t, "event_payment_success"), hookSecret)
	req.Headers["X-Idempotency-Key"] = []string{"n9rn7079wqXcse3GEDEXCYle9ajXmU0SUQY8zrUNAlc="}
	ns, err := c.ParseWebhook(context.Background(), req)
	if err != nil || len(ns) != 1 || ns[0].EventID != "cf-n9rn7079wqXcse3GEDEXCYle9ajXmU0SUQY8zrUNAlc=" {
		t.Fatalf("%+v %v", ns, err)
	}
}

func TestUnknownOrderIsNotFound(t *testing.T) {
	f := newFake()
	body := map[string]any{"code": "order_not_found", "message": "Order Reference Id does not exist", "type": "invalid_request_error"}
	f.reply("GET /pg/orders/nope", 404, body)
	if _, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "nope"}); !errors.Is(err, connector.ErrNotFound) {
		t.Fatal(err)
	}
}

// A retried checkout after the idempotency window finds the order already there
// (409 order_already_exists): the connector adopts it, after checking it is the
// same order, and returns its session.
func TestCreateCheckoutAdoptsAnExistingOrder(t *testing.T) {
	f := newFake()
	f.reply("POST /pg/orders", 409, map[string]any{"code": "order_already_exists", "message": "order with same id is already present", "type": "invalid_request_error"})
	f.reply("GET /pg/orders/pay_ref_01", 200, raw(t, "real_order"))
	c := newConn(t, f)
	req := checkoutReq()
	req.Amount = 10050
	res, err := c.CreateCheckout(context.Background(), req)
	if err != nil || res.ProviderOrderID != "pay_ref_01" || res.NextAction.Options["payment_session_id"] != "session_SANITIZED_opaque_tokenpayment" {
		t.Fatalf("%+v %v", res, err)
	}
	req.Amount = 99999 // an order of the same id but another amount is not ours
	if _, err := c.CreateCheckout(context.Background(), req); err == nil {
		t.Error("an existing order with another amount must be refused")
	}
}

// A refund_id that already exists (400 refund_id_invalid, "Duplicate Merchant
// Refund Id") is the same refund when it matches: a retried job gets it back.
func TestRefundAdoptsAnExistingRefund(t *testing.T) {
	f := newFake()
	f.reply("POST /pg/orders/pay_ref_01/refunds", 400, map[string]any{"code": "refund_id_invalid", "message": "Duplicate Merchant Refund Id", "type": "invalid_request_error"})
	f.reply("GET /pg/orders/pay_ref_01/refunds/rf_ref_01", 200, raw(t, "real_refund"))
	c := newConn(t, f).(connector.Refunder)
	req := connector.RefundRequest{ProviderPaymentID: "pay_ref_01:1465089236821754368", ProviderRef: "rf_ref_01", Amount: 1025, Currency: "INR"}
	snap, err := c.Refund(context.Background(), req)
	if err != nil || snap.ProviderRef != "rf_ref_01" || snap.Amount != 1025 {
		t.Fatalf("%+v %v", snap, err)
	}
	req.Amount = 5000
	_, err = c.Refund(context.Background(), req)
	var nr connector.ErrNotRefundable
	if err == nil || errors.As(err, &nr) {
		t.Errorf("a duplicate id with another amount is a conflict, not a plain not-refundable: %v", err)
	}
}
