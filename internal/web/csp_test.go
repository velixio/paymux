package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/web"
	"github.com/velixio/paymux/pkg/connector"
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
	p := web.HostedCSP("N0nce", connector.CheckoutHosts{})
	for name, want := range map[string]string{"default-src": "'self'", "script-src": "'self' 'nonce-N0nce'", "frame-src": "'none'",
		"form-action": "'self'", "base-uri": "'none'", "frame-ancestors": "'none'", "connect-src": "'self'"} {
		if got := directive(t, p, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if strings.Contains(p, "unsafe-eval") || strings.Contains(directive(t, p, "script-src"), "unsafe-inline") {
		t.Errorf("script policy too loose: %s", p)
	}
	if a, b := web.NewNonce(), web.NewNonce(); a == b || len(a) < 20 {
		t.Error("nonces must be random and long")
	}
}

// The policy lists the declared hosts and nothing else: the URLs of the action
// are allowed only because the connector declared them (the hub refuses an
// action outside them, NextAction.Allowed).
func TestHostedCSPHasOnlyTheDeclaredHosts(t *testing.T) {
	h := connector.CheckoutHosts{Script: []string{"https://sdk.example.com"}, Form: []string{"https://pay.example.com"},
		Frame: []string{"https://frames.example.com"}, Connect: []string{"https://api.example.com"}, Img: []string{"https://img.example.com"},
		Redirect: []string{"https://redirect.example.com"}}
	p := web.HostedCSP("n", h)
	for name, want := range map[string]string{"script-src": "'self' 'nonce-n' https://sdk.example.com", "form-action": "'self' https://pay.example.com",
		"frame-src": "https://frames.example.com", "connect-src": "'self' https://api.example.com", "img-src": "'self' data: https://img.example.com"} {
		if got := directive(t, p, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// A redirect target leaves the page: it is not a source of the policy.
	if strings.Contains(p, "redirect.example.com") {
		t.Errorf("redirect host in the policy: %s", p)
	}
	// An undeclared script origin is absent.
	if strings.Contains(p, "evil") || strings.Contains(directive(t, web.HostedCSP("n", connector.CheckoutHosts{}), "script-src"), "https://") {
		t.Errorf("undeclared host in the policy: %s", p)
	}
}

// The policy is enforced, reports violations to the hub, and keeps the
// framing ban in the same header.
func TestHostedCSPIsEnforcedAndReports(t *testing.T) {
	if web.HostedCSPHeader != "Content-Security-Policy" {
		t.Fatalf("header = %s", web.HostedCSPHeader)
	}
	p := web.HostedCSP("n", connector.CheckoutHosts{})
	if directive(t, p, "report-uri") != "/csp-report" || directive(t, p, "frame-ancestors") != "'none'" {
		t.Errorf("policy = %s", p)
	}
	hdr := http.Header{}
	web.SetHostedCSP(hdr, &web.PayPage{})
	if got := hdr.Values("Content-Security-Policy"); len(got) != 1 || !strings.Contains(got[0], "script-src") || !strings.Contains(got[0], "frame-ancestors 'none'") {
		t.Errorf("headers = %q", got)
	}
	if hdr.Get("Content-Security-Policy-Report-Only") != "" {
		t.Error("a report-only policy is still sent")
	}
}

func TestHostedCSPDropsMalformedConnectorSources(t *testing.T) {
	registerHostile() // declares keywords, schemes, paths, spaces and a smuggled directive
	_, info, _ := connector.Lookup("hostile")
	p := web.HostedCSP("n", *info.CheckoutHosts)
	for name, want := range map[string]string{"script-src": "'self' 'nonce-n' https://good.example.com", "frame-src": "'none'", "connect-src": "'self'"} {
		if got := directive(t, p, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if strings.Count(p, "report-uri") != 1 {
		t.Errorf("a connector smuggled a directive: %s", p)
	}
}
