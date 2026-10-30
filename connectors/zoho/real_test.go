package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// The testdata files real_*, err_* and event_* follow responses recorded from the
// Zoho Payments API (payments.zoho.in): the shapes are real, the ids, people and
// references are replaced. The tests pin what the real API does where the first
// version of the connector, written from the documentation, did not.

func raw(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rfake serves fixed answers per "METHOD /path" and records the bodies it saw.
type rfake struct {
	mu     sync.Mutex
	routes map[string]rreply
	bodies map[string][]byte
	calls  []string
}

type rreply struct {
	status int
	body   []byte
}

func newRFake() *rfake { return &rfake{routes: map[string]rreply{}, bodies: map[string][]byte{}} }

func (f *rfake) on(route string, status int, body []byte) { f.routes[route] = rreply{status, body} }

func (f *rfake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/oauth/v2/token" {
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, route)
	f.bodies[route] = b
	rep, ok := f.routes[route]
	if !ok {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":"error","message":"no route"}`))
		return
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.body)
}

func (f *rfake) conn(t testing.TB, cfg json.RawMessage) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(cfg, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ---- errors: the API answers 400 with a string code, not 404 ----

func TestRealUnknownIDsAreNotFound(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/payments/9", 400, raw(t, "err_unknown_payment"))
	f.on("GET /api/v1/paymentsessions/9", 400, raw(t, "err_unknown_session"))
	f.on("GET /api/v1/refunds/9", 400, raw(t, "err_unknown_payment"))
	f.on("GET /api/v1/mandates/9", 400, []byte(`{"code":"resource_does_not_exist","message":"Mandate Reference does not exist."}`))
	c := f.conn(t, widgetCfg)
	ctx := context.Background()
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "9"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("payment: %v", err)
	}
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: "9"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("session: %v", err)
	}
	if _, err := c.(connector.Refunder).FetchRefund(ctx, connector.RefundRef{ProviderRefundID: "9"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("refund: %v", err)
	}
	if _, err := c.(connector.MandateManager).FetchMandate(ctx, connector.MandateRef{ProviderMandateID: "9"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("mandate: %v", err)
	}
}

// A token without the scope for an endpoint gets 401 "Not An Authorized User":
// the credentials are not what is wrong, a scope is missing, and the message says so.
func TestRealMissingScopeIsReportedAsInvalidCredentials(t *testing.T) {
	f := newRFake()
	f.on("GET /api/v1/payments/9", 401, raw(t, "err_not_authorised"))
	_, err := f.conn(t, widgetCfg).FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "9"})
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("%v", err)
	}
}

// ---- payment session ----

// The API rejects a return_url on a payment session ("Invalid data provided."):
// the return URL is a widget option only.
func TestRealSessionBodyHasNoReturnURL(t *testing.T) {
	f := newRFake()
	f.on("POST /api/v1/paymentsessions", 201, raw(t, "real_session_created"))
	c := f.conn(t, widgetCfg)
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 10050, Currency: "INR", Description: "Pro"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/paymentsessions"], &body)
	if _, ok := body["return_url"]; ok {
		t.Errorf("return_url must not be sent: %v", body)
	}
	if body["description"] != "Pro" || body["reference_number"] != "pay_ref_01" {
		t.Errorf("%v", body)
	}
	if res.ProviderOrderID != "28000000000219002" || res.ExpiresAt != 1792284724000 {
		t.Errorf("%+v", res)
	}
	// The widget needs the description (required by requestPaymentMethod) and the return URL.
	o := res.NextAction.Options
	if o["description"] != "Pro" || o["return_url"] != "https://pay.example.com/return/acc/pay_ref_01" || o["domain"] != "IN" {
		t.Errorf("%v", o)
	}
}

// reference_number is limited to 50 characters; a longer one is refused before
// any request (the API's answer is a plain 400).
func TestRealSessionRefusesAnOverlongReference(t *testing.T) {
	f := newRFake()
	c := f.conn(t, widgetCfg)
	long := make([]byte, 51)
	for i := range long {
		long[i] = 'r'
	}
	if _, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: string(long), Amount: 100, Currency: "INR"}); err == nil || len(f.calls) != 0 {
		t.Errorf("err=%v calls=%v", err, f.calls)
	}
}
