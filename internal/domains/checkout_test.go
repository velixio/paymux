package domains

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"
)

func checkoutRouter() *Router {
	r := newRouter()
	r.CheckoutHost = "checkout.example.com"
	r.CheckoutOrigin = "https://checkout.example.com"
	r.Checkout = okHandler("checkout")
	return r
}

// B-7c: with a checkout origin the customer pages are served on that host
// only, and the base host sends them there.
func TestCheckoutHostServesOnlyCustomerRoutes(t *testing.T) {
	r := checkoutRouter()
	for _, p := range []string{"/pay/pay_1", "/l/slug", "/s/sub_1", "/i/inv_1.pdf", "/return/pa_1/REF", "/sim/REF", "/assets/css/app.css", "/theme/x/y.css", "/csp-report"} {
		if w := do(r, "checkout.example.com", p); w.Code != 200 || w.Body.String() != "checkout:" {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/dashboard/", "/v1/payments", "/hooks/pa_1", "/setup", "/docs", "/", "/payx", "/.well-known/paymux/tok", "/favicon.ico"} {
		if w := do(r, "checkout.example.com:443", p); w.Code != 404 {
			t.Errorf("%s on the checkout host: got %d, want 404", p, w.Code)
		}
	}
}

func TestBaseHostRedirectsCustomerRoutesToTheCheckoutHost(t *testing.T) {
	r := checkoutRouter()
	for path, want := range map[string]string{
		"/pay/pay_1?t=sec%20ret&lang=hi": "https://checkout.example.com/pay/pay_1?t=sec%20ret&lang=hi",
		"/l/spring":                      "https://checkout.example.com/l/spring",
		"/s/sub_1?t=x":                   "https://checkout.example.com/s/sub_1?t=x",
		"/i/inv_1.pdf?t=x":               "https://checkout.example.com/i/inv_1.pdf?t=x",
		"/return/pa_1/REF?x=1":           "https://checkout.example.com/return/pa_1/REF?x=1",
		"/sim/REF?pa=pa_1":               "https://checkout.example.com/sim/REF?pa=pa_1",
	} {
		w := do(r, "pay.example.com", path)
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != want {
			t.Errorf("%s: %d %q, want 308 %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	// Everything the dashboard needs stays on the base host.
	for _, p := range []string{"/v1/payments", "/dashboard/", "/hooks/pa_1", "/assets/css/app.css", "/theme/x/y.css", "/setup", "/payx", "/csp-report"} {
		if w := do(r, "pay.example.com", p); w.Code != 200 || w.Body.String() != "base:" {
			t.Errorf("%s on the base host: %d %q", p, w.Code, w.Body.String())
		}
	}
	// Custom domains keep working.
	if w := do(r, "pay.voicestudio.example", "/pay/pay_1"); w.Body.String() != "cust:app_vs" {
		t.Errorf("custom domain: %q", w.Body.String())
	}
}

func TestTLSAllowAllowsTheCheckoutHost(t *testing.T) {
	nowhere := func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	h := NewTLSAllow(TLSAllowOptions{BaseHost: "pay.example.com", ExtraHosts: []string{"checkout.example.com"}, Lookup: newRouter().Lookup, Resolve: nowhere})
	if got := ask(h, "checkout.example.com"); got != 200 {
		t.Errorf("checkout host: %d", got)
	}
	if got := ask(h, "other.example.com"); got != 403 {
		t.Errorf("other: %d", got)
	}
}
