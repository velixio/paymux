package zoho

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// AUDIT: a payment fetched by an outside (unsigned return URL) payment id that
// names NO session and NO reference is accepted as ours, and is even stamped
// with our session id, so the core's snapshotIsOurs check cannot catch it.
// Expected: ErrNotFound (nothing ties the payment to the attempt).
func TestAuditPayZohoFetchByIDAcceptsPaymentWithNoSessionOrReference(t *testing.T) {
	f := newFake("succeeded")
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v1/payments/pay_foreign" {
			// A real, succeeded payment of the same Zoho account made outside this
			// checkout (payment link, invoice, mandate registration...).
			write(w, 200, map[string]any{"code": 0, "payment": map[string]any{"payment_id": "pay_foreign",
				"amount": 499.00, "currency": "INR", "status": "succeeded", "payment_method": "upi"}})
			return
		}
		f.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(widgetCfg, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{
		ProviderRef: "pay_ref_01", ProviderOrderID: "ps_1", ProviderPaymentID: "pay_foreign"})
	if !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("a payment with no session and no reference was accepted for attempt ps_1/pay_ref_01: %+v (err %v)", snap, err)
	}
}
