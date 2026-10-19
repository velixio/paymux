package domains

import (
	"context"
	"errors"
	"net/netip"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeLookup map[string]Domain

func (f fakeLookup) LookupHost(_ context.Context, host string) (Domain, bool, error) {
	d, ok := f[host]
	return d, ok, nil
}

func okHandler(tag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, _ := AppFrom(r.Context())
		w.Write([]byte(tag + ":" + app))
	})
}

func newRouter() *Router {
	return &Router{
		BaseHost: "pay.example.com",
		Lookup: fakeLookup{
			"pay.voicestudio.example": {AppID: "app_vs", Status: StatusActive},
			"pay.pending.example":     {AppID: "app_p", Status: StatusPending},
		},
		Base:     okHandler("base"),
		Customer: okHandler("cust"),
	}
}

func do(h http.Handler, host, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBaseHostServesEverything(t *testing.T) {
	for _, p := range []string{"/v1/payments", "/dashboard", "/hooks/pa_1", "/pay/pay_1"} {
		if w := do(newRouter(), "pay.example.com:443", p); w.Code != 200 || w.Body.String() != "base:" {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
}

func TestCustomHostOnlyCustomerRoutes(t *testing.T) {
	r := newRouter()
	for _, p := range []string{"/pay/pay_1", "/return/pa_1/REF", "/i/inv_1", "/i/inv_1.pdf", "/s/sub_1", "/assets/css/app.css", "/.well-known/paymux/tok"} {
		if w := do(r, "pay.voicestudio.example", p); w.Code != 200 || w.Body.String() != "cust:app_vs" {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/v1/payments", "/dashboard", "/hooks/pa_1", "/healthz", "/", "/payx"} {
		if w := do(r, "pay.voicestudio.example", p); w.Code != 404 {
			t.Errorf("%s on custom host: got %d, want 404", p, w.Code)
		}
	}
}

func TestPendingDomainServesCustomerRoutes(t *testing.T) {
	if w := do(newRouter(), "pay.pending.example", "/.well-known/paymux/tok"); w.Code != 200 {
		t.Fatalf("pending domain verification route: %d", w.Code)
	}
}

func TestUnknownHost421(t *testing.T) {
	if w := do(newRouter(), "evil.example", "/pay/pay_1"); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("got %d", w.Code)
	}
}

func TestHostCaseAndPort(t *testing.T) {
	if w := do(newRouter(), "PAY.VoiceStudio.example:8443", "/pay/x"); w.Body.String() != "cust:app_vs" {
		t.Fatalf("got %q", w.Body.String())
	}
}

func TestTLSAllow(t *testing.T) {
	nowhere := func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	h := NewTLSAllow(TLSAllowOptions{BaseHost: "pay.example.com", Lookup: newRouter().Lookup, Resolve: nowhere})
	cases := map[string]int{
		"pay.example.com":         200,
		"pay.voicestudio.example": 200,
		"pay.pending.example":     403, // pending and not pointed at the hub (see tlsask_test.go)
		"evil.example":            403,
		"":                        400,
	}
	for d, want := range cases {
		r := httptest.NewRequest("GET", "/internal/tls-allow?domain="+d, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: got %d want %d", d, w.Code, want)
		}
	}
}

func TestFailedDomainNotServed(t *testing.T) {
	r := newRouter()
	r.Lookup = fakeLookup{"pay.bad.example": {AppID: "a", Status: StatusFailed}}
	if w := do(r, "pay.bad.example", "/pay/x"); w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("failed domain served: %d", w.Code)
	}
}
