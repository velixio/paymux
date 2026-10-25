package paylinks_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Audit H-7: the payment link page and its result pages send the nonce policy
// in report-only mode, with the enforced frame-ancestors beside it.
func TestLinkPagesSendACSP(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(nil)
		for what, path := range map[string]string{"link": "/l/" + l.Slug, "unknown link": "/l/nonesuch"} {
			r := h.do(h.browser(), "GET", path, "", nil)
			p := r.header.Get("Content-Security-Policy-Report-Only")
			if !regexp.MustCompile(`script-src 'self' 'nonce-[A-Za-z0-9_-]{20,}'; `).MatchString(p) || !strings.Contains(p, "frame-ancestors 'none'") {
				t.Errorf("%s: report-only policy = %q", what, p)
			}
			if got := r.header.Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
				t.Errorf("%s: enforced CSP = %q", what, got)
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
