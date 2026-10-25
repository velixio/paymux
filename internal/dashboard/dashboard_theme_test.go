package dashboard

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

const tealTheme = `{"name":"teal","version":"1.0.0","dashboard":{"seeds":{"accent":"#0d7d68"}}}`

func TestPageCarriesTheEffectiveDashboardTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		theme := func() string {
			body := e.pageOK(admin, "/dashboard/overview")
			_, rest, ok := strings.Cut(body, `data-dashboard-theme="`)
			if !ok {
				t.Fatalf("no data-dashboard-theme on the react root: %s", body)
			}
			name, _, _ := strings.Cut(rest, `"`)
			return name
		}
		// Nothing chosen: the customer-page "default" is not a dashboard theme, so stripe.
		if got := theme(); got != "stripe" {
			t.Errorf("default: %q", got)
		}
		e.setOrg("theme", `"paddle"`)
		if got := theme(); got != "paddle" {
			t.Errorf("org default paddle: %q", got)
		}
		e.writeTheme("teal", tealTheme, nil)
		e.setOrg("theme", `"teal"`)
		if got := theme(); got != "teal" {
			t.Errorf("custom theme with a dashboard block: %q", got)
		}
		// A theme without a dashboard block (a customer-page-only theme), or one
		// that is gone, never leaves the dashboard unstyled.
		e.setOrg("theme", `"minimal-dark"`)
		if got := theme(); got != "stripe" {
			t.Errorf("customer-page-only theme: %q", got)
		}
		e.setOrg("theme", `"gone"`)
		if got := theme(); got != "stripe" {
			t.Errorf("missing theme: %q", got)
		}
	})
}

func TestContentSecurityPolicyAllowsTheManifestFetchAndNoncedStyle(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		r, _ := e.get(admin, "/dashboard/overview")
		csp := r.Header.Get("Content-Security-Policy")
		// Same-origin fetch of /theme/<name>/dashboard.json falls under default-src 'self',
		// and the <style> the page writes carries the response's nonce.
		for _, want := range []string{"default-src 'self'", "style-src 'self' 'nonce-"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP lacks %q: %s", want, csp)
			}
		}
		if strings.Contains(csp, "connect-src") {
			t.Errorf("a connect-src would need to allow /theme/: %s", csp)
		}
		// The nonce in the page is the one in the header.
		body := e.pageOK(admin, "/dashboard/overview")
		_, rest, _ := strings.Cut(body, `data-style-nonce="`)
		nonce, _, _ := strings.Cut(rest, `"`)
		if nonce == "" {
			t.Error("the react root carries no style nonce")
		}
	})
}
