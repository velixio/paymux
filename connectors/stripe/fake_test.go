package stripe

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const (
	testKey    = "sk_test_known_key_123"
	testSecret = "whsec_test_known"
	nowUnix    = int64(1760000100)
)

var testConfig = json.RawMessage(`{"secret_key":"` + testKey + `","webhook_secret":"` + testSecret + `"}`)

// call is one request the fake saw.
type call struct {
	Method, Path string
	Query, Form  url.Values
	Header       http.Header
	Body         []byte
}

// fake is a Stripe API: routes are registered per "METHOD /path".
type fake struct {
	mu     sync.Mutex
	calls  []call
	routes map[string]func(w http.ResponseWriter, c call)
}

func newFake() *fake { return &fake{routes: map[string]func(http.ResponseWriter, call){}} }

func (f *fake) on(route string, fn func(w http.ResponseWriter, c call)) { f.routes[route] = fn }

// reply registers a fixed JSON answer.
func (f *fake) reply(route string, status int, body any) {
	f.on(route, func(w http.ResponseWriter, _ call) { write(w, status, body) })
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func stripeErr(w http.ResponseWriter, status int, typ, code, msg string) {
	write(w, status, map[string]any{"error": map[string]any{"type": typ, "code": code, "message": msg}})
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		c.Form, _ = url.ParseQuery(string(body))
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	fn := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		stripeErr(w, 401, "invalid_request_error", "", "Invalid API Key provided")
		return
	}
	if fn == nil {
		stripeErr(w, 404, "invalid_request_error", "resource_missing", "No such route "+r.URL.Path)
		return
	}
	fn(w, c)
}

func (f *fake) to(route string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method+" "+c.Path == route {
			out = append(out, c)
		}
	}
	return out
}

func (f *fake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func newConn(t *testing.T, f *fake) *stripe {
	t.Helper()
	c, err := New(testConfig, connectorDepsFor(t, f))
	if err != nil {
		t.Fatal(err)
	}
	return c.(*stripe)
}

// ---- canned objects ----

func sessionJSON(id, ref, status, payStatus string, pi any) map[string]any {
	return map[string]any{"id": id, "object": "checkout.session", "mode": "payment", "status": status, "payment_status": payStatus,
		"client_reference_id": ref, "amount_total": 49900, "currency": "inr", "payment_intent": pi, "created": nowUnix - 60,
		"url": "https://checkout.stripe.com/c/pay/" + id, "expires_at": nowUnix + 86000}
}

func intentJSON(id, status, ref string, charge any) map[string]any {
	return map[string]any{"id": id, "object": "payment_intent", "status": status, "amount": 49900, "currency": "inr", "created": nowUnix - 60,
		"metadata": map[string]string{"paymux_ref": ref}, "latest_charge": charge}
}

func chargeJSON(method string, refunded int64) map[string]any {
	return map[string]any{"id": "ch_3A1", "object": "charge", "amount_refunded": refunded, "payment_method_details": map[string]any{"type": method}}
}

func connectorDeps() connector.Deps { return connectortest.NewDeps(nil, time.Unix(nowUnix, 0)) }

func connectorDepsFor(t *testing.T, f *fake) connector.Deps {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return connectortest.NewDeps(srv, time.Unix(nowUnix, 0))
}
