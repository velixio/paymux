package whop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Whop ignores the checkout_configuration_id filter on GET /payments and
// returns the company's payments (seen in the sandbox, 2026-10-17). An unpaid
// checkout must never pick up another customer's paid payment.
func TestFetchPaymentNeverMatchesAnotherCheckout(t *testing.T) {
	other := map[string]any{"id": "pay_other", "status": "paid", "substatus": "succeeded", "currency": "usd", "total": "10.00",
		"checkout_configuration_id": "ch_OTHER", "metadata": map[string]string{"ref": "OTHERREF"}}
	mine := map[string]any{"id": "pay_mine", "status": "paid", "substatus": "succeeded", "currency": "usd", "total": "5.00",
		"checkout_configuration_id": "ch_MINE", "metadata": map[string]string{"ref": "MYREF"}}
	serve := func(payments []map[string]any) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/payments":
				_ = json.NewEncoder(w).Encode(map[string]any{"data": payments, "page_info": map[string]any{"has_next_page": false}})
			case "/api/v1/checkout_configurations/ch_MINE":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "ch_MINE", "plan": map[string]any{"initial_price": "5.00", "currency": "usd"}})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	check := func(payments []map[string]any, wantStatus connector.PaymentStatus, wantID string) {
		t.Helper()
		srv := serve(payments)
		defer srv.Close()
		c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ch_MINE", ProviderRef: "MYREF"})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status != wantStatus || s.ProviderPaymentID != wantID {
			t.Fatalf("got status %s id %q, want %s %q", s.Status, s.ProviderPaymentID, wantStatus, wantID)
		}
	}
	check([]map[string]any{other}, connector.PaymentRequiresAction, "")
	check([]map[string]any{other, mine}, connector.PaymentSucceeded, "pay_mine")
	// Matching by our metadata ref alone also counts (checkout id missing).
	mineNoCC := map[string]any{"id": "pay_mine2", "status": "paid", "substatus": "succeeded", "currency": "usd", "total": "5.00",
		"metadata": map[string]string{"ref": "MYREF"}}
	check([]map[string]any{other, mineNoCC}, connector.PaymentSucceeded, "pay_mine2")
}
