package web

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
)

// HostedCSPHeader is the header the hosted payment pages send their policy in.
// The policy is enforced (audit H-7): a provider host that is missing from the
// connector's CheckoutHosts blocks that script, frame or form post.
const HostedCSPHeader = "Content-Security-Policy"

// CSPReportPath is where browsers send violation reports (CSPReports).
const CSPReportPath = "/csp-report"

// NewNonce returns a fresh random nonce for one response.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HostedCSP returns the policy of a hosted payment page. The scripts of the
// page itself carry nonce. h is what the page's connector account declared
// (connector.HostsOf), the same hosts NextAction.Allowed holds the connector
// to, so the URLs of the action are allowed because they are declared, not
// because the connector returned them.
func HostedCSP(nonce string, h connector.CheckoutHosts) string {
	d := []string{
		"default-src 'self'",
		"script-src " + sources("'self' 'nonce-"+nonce+"'", h.Script),
		// The accent colour is an inline <style>; script is what the policy guards.
		"style-src 'self' 'unsafe-inline'",
		"img-src " + sources("'self' data:", h.Img),
		"frame-src " + sources("'none'", h.Frame),
		"connect-src " + sources("'self'", h.Connect),
		"form-action " + sources("'self'", h.Form),
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"report-uri " + CSPReportPath,
	}
	return strings.Join(d, "; ")
}

// sources joins base and the valid origins of list.
func sources(base string, list []string) string {
	out := base
	seen := map[string]bool{}
	for _, o := range list {
		if !connector.ValidOrigin(o) || seen[o] {
			continue
		}
		seen[o] = true
		if out == "'none'" {
			out = ""
		}
		out = strings.TrimSpace(out + " " + o)
	}
	return out
}

// SetHostedCSP stamps a response of a hosted payment page: it draws the nonce
// into data (for the page's scripts) and sets the enforced policy for the hosts
// in data.Hosts (empty for a page without a provider).
func SetHostedCSP(h http.Header, data *PayPage) {
	data.Nonce = NewNonce()
	h.Set(HostedCSPHeader, HostedCSP(data.Nonce, data.Hosts))
}
