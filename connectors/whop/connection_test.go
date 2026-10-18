package whop

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Whop looks a payment up by its own id or by a checkout configuration id, never
// by Paymux's reference alone, so the generic "fetch a payment that cannot exist"
// probe would fail before any request. It lists one payment instead.
func TestConnectionTester(t *testing.T) {
	f := newFake("")
	ct, ok := newConn(t, f).(connector.ConnectionTester)
	if !ok {
		t.Fatal("whop must implement ConnectionTester")
	}
	if err := ct.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[len(f.calls)-1]; got != "GET /api/v1/payments" {
		t.Errorf("one read-only call: %v", f.calls)
	}
	rejected := newConn(t, newFake(""))
	rejected.(*whop).cfg.APIKey = "wrong"
	if err := rejected.(connector.ConnectionTester).TestConnection(context.Background()); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("rejected key: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	down, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if err := down.(connector.ConnectionTester).TestConnection(context.Background()); err == nil || errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("an outage tells nothing about the key: %v", err)
	}
}
