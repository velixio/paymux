package whop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// What the Whop sandbox does (2026-10-18): a refund request over the remaining
// amount is not refused: it refunds what is left (partial_amount 999 on a $9.99
// payment with $1.50 refunded refunded $8.49) and answers 200; the response is
// the payment, and refund objects have ids "rf_...".
func TestRefundOverTheRemainderIsRefusedBeforeAnythingIsSent(t *testing.T) {
	var mu sync.Mutex
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/payments/pay_RealPay000001":
			_, _ = w.Write([]byte(`{"id":"pay_RealPay000001","status":"paid","substatus":"partially_refunded","currency":"usd","total":{"currency":"usd","amount":"9.99"},"refunded_amount":{"currency":"usd","amount":"1.50"},"metadata":{"ref":"X"}}`))
		case "POST /api/v1/payments/pay_RealPay000001/refund":
			posts++
			_, _ = w.Write([]byte(`{"id":"pay_RealPay000001","status":"paid","substatus":"partially_refunded","currency":"usd","total":{"currency":"usd","amount":"9.99"},"refunded_amount":{"currency":"usd","amount":"3.00"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	r := c.(connector.Refunder)
	var nr connector.ErrNotRefundable
	if _, err := r.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_a", ProviderPaymentID: "pay_RealPay000001", Amount: 850, Currency: "USD"}); !errors.As(err, &nr) || nr.Reason == "" {
		t.Errorf("$8.50 of the $8.49 left: %v", err)
	}
	if posts != 0 {
		t.Errorf("an over-refund reached Whop (%d)", posts)
	}
	if _, err := r.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_b", ProviderPaymentID: "pay_RealPay000001", Amount: 150, Currency: "USD"}); err != nil {
		t.Errorf("a refund within the remainder: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d", posts)
	}
}

func TestRefundObjectsHaveRfIDs(t *testing.T) {
	body, err := os.ReadFile("testdata/refund_real.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/refunds/rf_RealRefund0001" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.(connector.Refunder).FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rf_RealRefund0001", ProviderRef: "rf_01"})
	if err != nil || s.Status != connector.RefundSucceeded || s.Amount != 150 || s.Currency != "USD" || s.ProviderPaymentID != "pay_RealPay000001" || s.ProviderRef != "rf_01" {
		t.Errorf("%+v %v", s, err)
	}
}
