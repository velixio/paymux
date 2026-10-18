package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

func TestRealFetchPaymentByIDKeepsTheRealFields(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/payments/28000000000181002", 200, raw(t, "real_payment"))
	c := f.conn(t, widgetCfg)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "28000000000181002", ProviderRef: "pay_ref_01", ProviderOrderID: "28000000000219002"})
	if err != nil {
		t.Fatal(err)
	}
	// payment_method is an object ({"type":"upi",...}); "partially_refunded" is a paid
	// payment with part of it given back.
	if snap.Status != connector.PaymentSucceeded || snap.Method != "upi" || snap.Amount != 10050 || snap.AmountRefunded != 1000 || snap.Fee != 50 || snap.Tax != 9 ||
		snap.ProviderRef != "pay_ref_01" || snap.ProviderOrderID != "28000000000219002" || snap.ProviderPaymentID != "28000000000181002" {
		t.Errorf("%+v", snap)
	}
	for name, ref := range map[string]connector.PaymentRef{
		"another reference": {ProviderPaymentID: "28000000000181002", ProviderRef: "someone_else"},
		"another session":   {ProviderPaymentID: "28000000000181002", ProviderOrderID: "28000000000999999"},
	} {
		if _, err := c.FetchPayment(context.Background(), ref); !errors.Is(err, connector.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRealStatuses(t *testing.T) {
	for raw, want := range map[string]connector.PaymentStatus{
		"succeeded": connector.PaymentSucceeded, "refunded": connector.PaymentSucceeded, "partially_refunded": connector.PaymentSucceeded,
		"failed": connector.PaymentFailed, "initiated": connector.PaymentProcessing, "created": connector.PaymentProcessing,
	} {
		if got := mapPaymentStatus(raw); got != want {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
	for raw, want := range map[string]connector.RefundStatus{
		"succeeded": connector.RefundSucceeded, "failed": connector.RefundFailed, "canceled": connector.RefundFailed,
		"initiated": connector.RefundPending, "pending": connector.RefundPending,
	} {
		if got := mapRefundStatus(raw); got != want {
			t.Errorf("refund %s -> %s want %s", raw, got, want)
		}
	}
}

// A session lists its payments only as {payment_id, status, created_time}; the
// payment is then read, and it must belong to the session.
func TestRealFetchPaymentBySession(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/paymentsessions/28000000000219002", 200, raw(t, "real_session_get_paid"))
	f.on("GET /api/v1/payments/28000000000181002", 200, raw(t, "real_payment"))
	c := f.conn(t, widgetCfg)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "28000000000219002", ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderPaymentID != "28000000000181002" || snap.Amount != 10050 || snap.ProviderRef != "pay_ref_01" || snap.Method != "upi" {
		t.Fatalf("%+v %v", snap, err)
	}
	// The session answered for another reference.
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "28000000000219002", ProviderRef: "someone_else"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("another reference: %v", err)
	}
	// A payment that names another session is not this session's.
	other := newRFake()
	other.on("GET /api/v1/paymentsessions/28000000000219002", 200, raw(t, "real_session_get_paid"))
	var p map[string]any
	_ = json.Unmarshal(raw(t, "real_payment"), &p)
	p["payment"].(map[string]any)["payments_session_id"] = "28000000000555555"
	b, _ := json.Marshal(p)
	other.on("GET /api/v1/payments/28000000000181002", 200, b)
	if _, err := other.conn(t, widgetCfg).FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "28000000000219002"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("a payment of another session: %v", err)
	}
}

func TestRealFetchSessionNotPaidYet(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/paymentsessions/28000000000219002", 200, raw(t, "real_session_get_created"))
	c := f.conn(t, widgetCfg)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "28000000000219002", ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 10050 || snap.ProviderRef != "pay_ref_01" {
		t.Fatalf("%+v %v", snap, err)
	}
	// The session carries our reference_number: a session of another reference is not ours.
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "28000000000219002", ProviderRef: "someone_else"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("another reference: %v", err)
	}
}
