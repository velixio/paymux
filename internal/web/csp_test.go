package web_test

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/web"
)

// directive returns the sources of one directive of a policy.
func directive(t *testing.T, policy, name string) string {
	t.Helper()
	for _, d := range strings.Split(policy, ";") {
		if n, rest, _ := strings.Cut(strings.TrimSpace(d), " "); n == name {
			return rest
		}
	}
	t.Fatalf("no %s in %q", name, policy)
	return ""
}

// Audit H-7: the policy of a hosted payment page is nonce based and each
// connector's own hosts are in it; the hub's own pages stay on its origin.
func TestHostedCSPBase(t *testing.T) {
	p := web.HostedCSP("N0nce", "", "", "")
	for name, want := range map[string]string{"default-src": "'self'", "script-src": "'self' 'nonce-N0nce'", "frame-src": "'none'",
		"form-action": "'self'", "base-uri": "'none'", "frame-ancestors": "'none'", "connect-src": "'self'"} {
		if got := directive(t, p, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(p, "unsafe-eval") || strings.Contains(directive(t, p, "script-src"), "unsafe-inline") {
		t.Errorf("script policy too loose: %s", p)
	}
	if web.NewNonce() == web.NewNonce() || len(web.NewNonce()) < 20 {
		t.Error("nonces must be random and long")
	}
}

func TestHostedCSPAddsTheActionsOwnHosts(t *testing.T) {
	p := web.HostedCSP("n", "", "https://sdk.example.com/v1/a.js?x=1", "https://Pay.Example.com/_payment")
	if !strings.Contains(directive(t, p, "script-src"), " https://sdk.example.com") || !strings.Contains(directive(t, p, "form-action"), " https://pay.example.com") {
		t.Errorf("action hosts missing: %s", p)
	}
	// Relative and non-https URLs add nothing.
	p = web.HostedCSP("n", "", "http://sdk.example.com/a.js", "/pay/x")
	if strings.Contains(p, "example.com") {
		t.Errorf("non-https host allowed: %s", p)
	}
}

func TestHostedCSPDropsMalformedConnectorSources(t *testing.T) {
	registerHostile() // declares keywords, schemes, paths, spaces and a smuggled directive
	p := web.HostedCSP("n", "hostile", "", "")
	for name, want := range map[string]string{"script-src": "'self' 'nonce-n' https://good.example.com", "frame-src": "'none'", "connect-src": "'self'"} {
		if got := directive(t, p, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(p, "report-uri") {
		t.Errorf("a connector smuggled a directive: %s", p)
	}
}
