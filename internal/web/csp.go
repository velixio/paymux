package web

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
)

// HostedCSPHeader is the header the hosted payment pages send their policy in.
// It is report-only for now: a provider host that is missing from a connector's
// CheckoutHosts would otherwise break checkout. Once a release has run without
// violations it becomes "Content-Security-Policy" (audit H-7).
const HostedCSPHeader = "Content-Security-Policy-Report-Only"

// NewNonce returns a fresh random nonce for one response.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// originRE is the only shape of source a connector may declare: an https
// origin, optionally with a leading wildcard label. Anything else (keywords,
// schemes, paths, spaces, semicolons) is dropped, so a connector cannot widen
// the policy to 'unsafe-inline' or smuggle in another directive.
var originRE = regexp.MustCompile(`^https://(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)

// HostedCSP returns the policy of a hosted payment page. The scripts of the
// page itself carry nonce. connectorName (may be empty) selects the hosts the
// connector declared in Info.CheckoutHosts; scriptURL and formURL are the
// provider URLs this very page loads or posts to, allowed on top.
func HostedCSP(nonce, connectorName, scriptURL, formURL string) string {
	var h connector.CheckoutHosts
	if connectorName != "" {
		if _, info, ok := connector.Lookup(connectorName); ok && info.CheckoutHosts != nil {
			h = *info.CheckoutHosts
		}
	}
	script := append(append([]string{}, h.Script...), originOf(scriptURL))
	form := append(append([]string{}, h.Form...), originOf(formURL))
	d := []string{
		"default-src 'self'",
		"script-src " + sources("'self' 'nonce-"+nonce+"'", script),
		// The accent colour is an inline <style>; script is what the policy guards.
		"style-src 'self' 'unsafe-inline'",
		"img-src " + sources("'self' data:", h.Img),
		"frame-src " + sources("'none'", h.Frame),
		"connect-src " + sources("'self'", h.Connect),
		"form-action " + sources("'self'", form),
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}
	return strings.Join(d, "; ")
}

// sources joins base and the valid origins of list.
func sources(base string, list []string) string {
	out := base
	seen := map[string]bool{}
	for _, o := range list {
		if !originRE.MatchString(o) || seen[o] {
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

// originOf returns the https origin of raw, or "" for a relative or non-https URL.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return "https://" + strings.ToLower(u.Host)
}

// SetHostedCSP stamps a response of a hosted payment page: it draws the nonce
// into data (for the page's scripts), and sets the report-only policy for it
// and the enforced frame-ancestors, which a report-only policy cannot enforce.
// data.Launcher names the connector, data.ScriptURL and data.FormURL are the
// provider URLs of the page.
func SetHostedCSP(h http.Header, data *PayPage) {
	data.Nonce = NewNonce()
	h.Set(HostedCSPHeader, HostedCSP(data.Nonce, data.Launcher, data.ScriptURL, data.FormURL))
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
}
