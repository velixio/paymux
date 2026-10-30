package paylinks_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// Audit H-7: the payment link page and its result pages send the nonce policy
// enforced.
func TestLinkPagesSendACSP(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(nil)
		for what, path := range map[string]string{"link": "/l/" + l.Slug, "unknown link": "/l/nonesuch"} {
			r := h.do(h.browser(), "GET", path, "", nil)
			p := r.header.Get("Content-Security-Policy")
			if !regexp.MustCompile(`script-src 'self' 'nonce-[A-Za-z0-9_-]{20,}'; `).MatchString(p) || !strings.Contains(p, "frame-ancestors 'none'") || !strings.Contains(p, "report-uri /csp-report") {
				t.Errorf("%s: policy = %q", what, p)
			}
			if r.header.Get("Content-Security-Policy-Report-Only") != "" {
				t.Errorf("%s: a report-only policy is still sent", what)
			}
			if r.header.Get("X-Frame-Options") != "DENY" || r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Cache-Control") != "no-store" {
				t.Errorf("%s: hardening headers changed: %v", what, r.header)
			}
			if strings.Contains(r.body, "<script") {
				t.Errorf("%s: the page has a script of its own", what)
			}
		}
	})
}
