package subscriptions_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
)

var (
	nonceRE     = regexp.MustCompile(`script-src [^;]*'nonce-([A-Za-z0-9_-]{20,})'`)
	scriptTagRE = regexp.MustCompile(`<script[^>]*>`)
)

// wantHostedCSP: audit H-7 on the hosted subscription checkout. The policy is
// report-only with a nonce that is on every script; the enforced
// frame-ancestors stays.
func wantHostedCSP(t *testing.T, what string, h http.Header, body string, scripts int) string {
	t.Helper()
	policy := h.Get("Content-Security-Policy-Report-Only")
	m := nonceRE.FindStringSubmatch(policy)
	if m == nil {
		t.Fatalf("%s: no nonce policy in Content-Security-Policy-Report-Only %q", what, policy)
	}
	tags := scriptTagRE.FindAllString(body, -1)
	if len(tags) != scripts {
		t.Errorf("%s: %d script tags, want %d", what, len(tags), scripts)
	}
	for _, tag := range tags {
		if !strings.Contains(tag, ` nonce="`+m[1]+`"`) {
			t.Errorf("%s: script %s does not carry the nonce %s", what, tag, m[1])
		}
	}
	if got := h.Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("%s: enforced CSP = %q", what, got)
	}
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s: hardening headers changed: %v", what, h)
	}
	return policy
}

func TestHostedCheckoutSendsANonceCSP(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := hostedServer(e)

		// form_post (the mandate connector)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		_, body, hdr := fetch(t, srv.URL+u.RequestURI())
		p := wantHostedCSP(t, "form_post", hdr, body, 1)
		if !strings.Contains(p, "form-action 'self' https://pg.example.com") {
			t.Errorf("form_post: %s", p)
		}

		// sdk (Razorpay): its declared hosts are in the policy
		e.routeTo("razorpay")
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = ""; i.TotalCount = 12 })
		u, _ = url.Parse(c.CheckoutURL)
		_, body, hdr = fetch(t, srv.URL+u.RequestURI())
		p = wantHostedCSP(t, "sdk", hdr, body, 2)
		if !strings.Contains(p, "https://checkout.example.com") || !strings.Contains(p, "https://checkout.razorpay.com") || !strings.Contains(p, "https://api.razorpay.com") {
			t.Errorf("sdk: provider hosts missing: %s", p)
		}

		// the billing form
		c = e.mustCreate(func(i *subscriptions.CreateInput) { i.Customer = noBilling })
		u, _ = url.Parse(c.CheckoutURL)
		_, body, hdr = fetch(t, srv.URL+u.RequestURI())
		wantHostedCSP(t, "billing", hdr, body, 0)
	})
}
