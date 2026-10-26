package paypage_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/pkg/connector"
)

var (
	nonceRE     = regexp.MustCompile(`script-src [^;]*'nonce-([A-Za-z0-9_-]{20,})'`)
	scriptTagRE = regexp.MustCompile(`<script[^>]*>`)
)

// wantHostedCSP checks the audit H-7 headers of a hosted page: a report-only
// policy whose nonce is on every script of the body, plus the enforced
// frame-ancestors (the report-only header cannot enforce it).
func wantHostedCSP(t *testing.T, what string, h http.Header, body string, scripts int) string {
	t.Helper()
	policy := h.Get("Content-Security-Policy")
	m := nonceRE.FindStringSubmatch(policy)
	if m == nil {
		t.Fatalf("%s: no nonce policy in Content-Security-Policy %q", what, policy)
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
	if !strings.Contains(policy, "frame-ancestors 'none'") || !strings.Contains(policy, "report-uri /csp-report") {
		t.Errorf("%s: policy lacks frame-ancestors or report-uri: %s", what, policy)
	}
	if h.Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("%s: a report-only policy is still sent", what)
	}
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("%s: hardening headers changed: %v", what, h)
	}
	return policy
}

func TestHostedPagesSendANonceCSP(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{ProviderOrderID: "o", NextAction: connector.NextAction{Kind: connector.ActionFormPost,
					URL: "https://secure.payu.example.com/_payment", Fields: map[string]string{"txnid": "T1"}}}, nil
			}
		})
		r := e.do("GET", e.create(core.CreatePaymentInput{}).path(), "", nil)
		p := wantHostedCSP(t, "form_post", r.header, r.body, 1)
		if !strings.Contains(p, "form-action 'self' https://*.example.com") {
			t.Errorf("form_post: form host missing: %s", p)
		}

		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{ProviderOrderID: "o", NextAction: connector.NextAction{Kind: connector.ActionSDK,
					ScriptURL: "https://checkout.example.com/v1/checkout.js", Options: map[string]any{"key": "k", "order_id": "order_1", "amount": 1000}}}, nil
			}
		})
		r = e.do("GET", e.create(core.CreatePaymentInput{}).path(), "", nil)
		p = wantHostedCSP(t, "sdk", r.header, r.body, 2)
		// The hosts the connector declared (its account's CheckoutHosts), not the action's own URL.
		if !strings.Contains(p, "script-src 'self' 'nonce-") || !strings.Contains(p, " https://*.example.com") || !strings.Contains(p, "frame-src https://frames.fake.example.com") ||
			strings.Contains(p, "https://checkout.example.com") {
			t.Errorf("sdk: provider hosts missing: %s", p)
		}
		if a, b := nonceRE.FindStringSubmatch(p), nonceRE.FindStringSubmatch(e.do("GET", e.create(core.CreatePaymentInput{}).path(), "", nil).header.Get("Content-Security-Policy")); a[1] == b[1] {
			t.Error("the nonce repeats between responses")
		}

		// A page without any provider: result and billing form carry the policy too.
		r = e.do("GET", "/pay/pay_nonesuch?t=x", "", nil)
		wantHostedCSP(t, "not found", r.header, r.body, 0)
		// The return page of a provider is enforced too.
		r = e.do("GET", "/return/pa_nonesuch/ref_nonesuch", "", nil)
		wantHostedCSP(t, "return", r.header, r.body, 0)
		r = e.do("GET", e.inrPayment(50000).path(), "", nil)
		p = wantHostedCSP(t, "billing", r.header, r.body, 0)
		if strings.Contains(p, "https://") {
			t.Errorf("billing: no provider host expected: %s", p)
		}
	})
}
