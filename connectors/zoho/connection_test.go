package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// hostFake answers per Host header: the API of the live server, the API of the
// sandbox server, and the OAuth token endpoint. The test client sends every
// request to one server but keeps the Host.
type hostFake struct {
	mu       sync.Mutex
	live     int // status of GET /api/v1/payments on payments.zoho.in
	sandbox  int // ... on paymentssandbox.zoho.in
	tokenErr string
	calls    []string
}

func (f *hostFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Host+" "+r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/oauth/v2/token" {
		if f.tokenErr != "" {
			_, _ = w.Write([]byte(`{"error":"` + f.tokenErr + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		return
	}
	status := f.live
	if strings.HasPrefix(r.Host, "paymentssandbox.") {
		status = f.sandbox
	}
	if r.URL.Path != "/api/v1/payments" {
		w.WriteHeader(404)
		return
	}
	w.WriteHeader(status)
	switch status {
	case 200:
		_, _ = w.Write([]byte(`{"code":0,"message":"success","payments":[]}`))
	case 401:
		_, _ = w.Write([]byte(`{"code":"error","message":"Not An Authorized User"}`))
	default:
		_, _ = w.Write([]byte(`{"code":"error","message":"boom"}`))
	}
}

func (f *hostFake) tester(t *testing.T, cfg string) connector.ConnectionTester {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(json.RawMessage(cfg), connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	ct, ok := c.(connector.ConnectionTester)
	if !ok {
		t.Fatal("zoho must implement ConnectionTester: it cannot fetch a payment by Paymux's reference")
	}
	return ct
}

const (
	liveCfg    = `{"account_id":"acc1","client_id":"c","client_secret":"s","refresh_token":"r","webhook_secret":"w","widget_api_key":"k"}`
	sandboxCfg = `{"account_id":"acc1","client_id":"c","client_secret":"s","refresh_token":"r","webhook_secret":"w","widget_api_key":"k","sandbox":true}`
	freshCfg   = `{"account_id":"acc1","client_id":"c","client_secret":"s","webhook_secret":"w","widget_api_key":"k"}`
)

func TestConnectionAcceptedWithAReadOnlyCall(t *testing.T) {
	f := &hostFake{live: 200}
	if err := f.tester(t, liveCfg).TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1]
	if !strings.HasPrefix(last, "payments.zoho.in GET /api/v1/payments?") || !strings.Contains(last, "per_page=1") || !strings.Contains(last, "account_id=acc1") {
		t.Errorf("one cheap GET with the scopes the connector already has: %v", f.calls)
	}
	sb := &hostFake{sandbox: 200}
	if err := sb.tester(t, sandboxCfg).TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sb.calls[len(sb.calls)-1]; !strings.HasPrefix(got, "paymentssandbox.zoho.in GET /api/v1/payments?") {
		t.Errorf("the sandbox setting uses the sandbox host: %v", sb.calls)
	}
}

func TestConnectionNotConnectedYet(t *testing.T) {
	f := &hostFake{live: 200}
	err := f.tester(t, freshCfg).TestConnection(context.Background())
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("%v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("nothing to ask Zoho yet: %v", f.calls)
	}
}

func TestConnectionRefreshTokenRefused(t *testing.T) {
	f := &hostFake{live: 200, tokenErr: "invalid_code"}
	err := f.tester(t, liveCfg).TestConnection(context.Background())
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "accounts.zoho.in") || !strings.Contains(err.Error(), "data centre") {
		t.Fatalf("the message names the accounts host that was asked and the usual causes: %v", err)
	}
}

// The mistake this exists for: a sandbox account's token (ZohoPaySandbox.* scopes)
// used on the live host, or the reverse. Zoho answers 401 "Not An Authorized User"
// on the wrong server, so the other server is asked before blaming the token.
func TestConnectionWrongServer(t *testing.T) {
	f := &hostFake{live: 401, sandbox: 200}
	err := f.tester(t, liveCfg).TestConnection(context.Background())
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "sandbox") || !strings.Contains(err.Error(), "Sandbox setting") {
		t.Fatalf("live setting, sandbox token: %v", err)
	}
	g := &hostFake{live: 200, sandbox: 401}
	err = g.tester(t, sandboxCfg).TestConnection(context.Background())
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "live") || !strings.Contains(err.Error(), "Sandbox setting") {
		t.Fatalf("sandbox setting, live token: %v", err)
	}
	h := &hostFake{live: 401, sandbox: 401}
	err = h.tester(t, liveCfg).TestConnection(context.Background())
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "ZohoPay.payments.READ") {
		t.Fatalf("neither server accepts it: %v", err)
	}
}

func TestConnectionOutageIsInconclusive(t *testing.T) {
	f := &hostFake{live: 500}
	err := f.tester(t, liveCfg).TestConnection(context.Background())
	if err == nil || errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("%v", err)
	}
}
