package connectortest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// A connector's conformance test is one call to Run. It has no Output comment
// because it needs a *testing.T: compiled here, run from the connector's own
// test file.
func ExampleRun() {
	provider := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the fake provider: answer CreateCheckout and FetchPayment here
	})
	_ = func(t *testing.T) {
		connectortest.Run(t, newAcme, connectortest.Fixtures{
			Config:          []byte(`{"api_key":"k","webhook_secret":"s"}`),
			Ref:             connector.PaymentRef{ProviderRef: "pay_ref_01"},
			Checkout:        connector.CheckoutRequest{PaymentID: "pay_1", AttemptID: "att_1", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "USD"},
			CheckoutHandler: provider,
			// Statuses, Webhooks and Return carry the provider's fixtures.
		})
	}
}

func newAcme(json.RawMessage, connector.Deps) (connector.Connector, error) { return nil, nil }

// NewDeps wires a connector to a fake provider: every request, whatever host
// the connector builds, is served by the test server.
func ExampleNewDeps() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "fake saw %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	deps := connectortest.NewDeps(srv, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	resp, err := deps.HTTP.Get("https://api.provider.example/v1/payments/p1")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	fmt.Println(string(buf[:n]))
	fmt.Println(deps.URLs.ReturnURL("ref 1"))
	fmt.Println(deps.URLs.WebhookURL())
	// Output:
	// fake saw GET /v1/payments/p1
	// https://pay.example.com/return/acc/ref%201
	// https://pay.example.com/hooks/acc
}

// SampleValues lists one populated value of every SDK type; the conformance
// kit round-trips them through JSON to prove the types stay serializable.
func ExampleSampleValues() {
	fmt.Println(len(connectortest.SampleValues()) > 20)
	// Output: true
}
