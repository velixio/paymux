package paymux

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New("pk_test_secret", WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCreatePayment(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/payments" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer pk_test_secret" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "order-1-payment" {
			t.Errorf("idempotency = %q", got)
		}
		var body CreatePaymentParams
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Amount != 49900 || body.Currency != "INR" {
			t.Errorf("body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pay_1","object":"payment","status":"created","checkout_url":"https://pay.example/pay/pay_1?t=x","expires_at":"2026-10-30T12:00:00Z","created_at":"2026-10-30T11:00:00Z"}`))
	})
	payment, err := client.Payments.Create(context.Background(), CreatePaymentParams{Amount: 49900, Currency: "INR"}, WithIdempotencyKey("order-1-payment"))
	if err != nil {
		t.Fatal(err)
	}
	if payment.ID != "pay_1" || payment.CheckoutURL == "" {
		t.Fatalf("payment = %+v", payment)
	}
}

func TestMutationRequiresIdempotencyKey(t *testing.T) {
	client, err := New("key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Payments.Create(context.Background(), CreatePaymentParams{}); err == nil {
		t.Fatal("mutation accepted without key")
	}
}

func TestCheckoutAction(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/payments/pay_1/checkout-action" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "order-1-checkout" {
			t.Errorf("idempotency = %q", got)
		}
		_, _ = w.Write([]byte(`{"kind":"checkout","payment_id":"pay_1","status":"created","launcher":"razorpay","action":{"kind":"sdk","script_url":"https://checkout.razorpay.com/v1/checkout.js","options":{"key":"public"}}}`))
	})
	action, err := client.Payments.CheckoutAction(context.Background(), "pay_1", CheckoutActionParams{}, WithIdempotencyKey("order-1-checkout"))
	if err != nil {
		t.Fatal(err)
	}
	if action.PaymentID != "pay_1" || action.Launcher != "razorpay" || action.Action.Kind != "sdk" {
		t.Fatalf("action = %+v", action)
	}
}

func TestListQuery(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "10" {
			t.Errorf("limit = %q", got)
		}
		if got := r.URL.Query().Get("starting_after"); got != "pay_1" {
			t.Errorf("cursor = %q", got)
		}
		if got := r.URL.Query().Get("status"); got != "succeeded" {
			t.Errorf("status = %q", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[],"has_more":false}`))
	})
	if _, err := client.Payments.List(context.Background(), ListPaymentsParams{Limit: 10, StartingAfter: "pay_1", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req_1")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limited","message":"slow down","request_id":"req_body"}}`))
	})
	_, err := client.Payments.Get(context.Background(), "pay_1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v", err, err)
	}
	if apiErr.Type != "rate_limited" || apiErr.RequestID != "req_body" || !apiErr.Temporary() {
		t.Fatalf("API error = %+v", apiErr)
	}
}

func TestBaseURLSafety(t *testing.T) {
	if _, err := New("key", WithBaseURL("http://payments.example.com")); err == nil {
		t.Fatal("non-loopback HTTP accepted")
	}
	if _, err := New("key", WithBaseURL("http://127.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if _, err := New("key", WithBaseURL("https://user:pass@example.com")); err == nil {
		t.Fatal("URL credentials accepted")
	}
}
