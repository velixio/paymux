package stripe

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// Stripe never searches for a payment by reference (a checkout session id is
// needed), so the generic "fetch a payment that cannot exist" probe fails before
// any request is made. It lists one checkout session instead.
func TestConnectionTester(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/checkout/sessions", 200, map[string]any{"object": "list", "data": []any{}, "has_more": false})
	s := newConn(t, f)
	ct, ok := connector.Connector(s).(connector.ConnectionTester)
	if !ok {
		t.Fatal("stripe must implement ConnectionTester")
	}
	if err := ct.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := f.to("GET /v1/checkout/sessions")
	if len(calls) != 1 || calls[0].Query.Get("limit") != "1" {
		t.Errorf("one cheap read-only call: %+v", calls)
	}
	s.api.key = "sk_test_wrong"
	if err := ct.TestConnection(context.Background()); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("a rejected key: %v", err)
	}
}

func TestConnectionTesterOutageIsInconclusive(t *testing.T) {
	f := newFake()
	f.on("GET /v1/checkout/sessions", func(w http.ResponseWriter, c call) { stripeErr(w, 503, "api_error", "", "down") })
	err := connector.Connector(newConn(t, f)).(connector.ConnectionTester).TestConnection(context.Background())
	if err == nil || errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("%v", err)
	}
}
