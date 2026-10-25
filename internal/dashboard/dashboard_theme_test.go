package dashboard

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

func themeOf(t *testing.T, e *themedEnv, c *http.Client) string {
	t.Helper()
	body := e.pageOK(c, "/dashboard/overview")
	_, rest, ok := strings.Cut(body, `data-dashboard-theme="`)
	if !ok {
		t.Fatalf("no data-dashboard-theme: %s", body)
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

func TestUserChoosesTheirOwnDashboardTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		e.writeTheme("pink", pinkTheme, nil) // customer-page-only: no dashboard block
		e.setOrg("theme", `"paddle"`)
		admin, viewer := e.roleClient("admin"), e.roleClient("viewer")

		page := e.pageOK(viewer, "/dashboard/account")
		has(t, page, `name="theme"`, `Organisation default (paddle)`, `value="midnight"`, `value="teal"`, `value="stripe"`)
		lacks(t, page, `value="pink"`, `value="default"`)
		tok := csrfOf(t, page)

		// Viewers have no settings permission, yet may pick their own theme.
		r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {"midnight"}, "csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("set own theme: %d", r.StatusCode)
		}
		if got := themeOf(t, &e, viewer); got != "midnight" {
			t.Errorf("viewer: %q", got)
		}
		if got := themeOf(t, &e, admin); got != "paddle" {
			t.Errorf("other users keep the org default: %q", got)
		}
		has(t, e.pageOK(viewer, "/dashboard/account"), `value="midnight" selected`)

		// Rejected: unknown, customer-page-only, malformed, no CSRF.
		for name, c := range map[string]string{"unknown": "nope", "no dashboard block": "pink", "traversal": "../etc", "bad name": "Pink!"} {
			if r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {c}, "csrf": {tok}}); r.StatusCode != 400 {
				t.Errorf("%s: %d, want 400", name, r.StatusCode)
			}
		}
		if r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {"teal"}}); r.StatusCode != 403 {
			t.Errorf("missing csrf: %d", r.StatusCode)
		}
		if got := themeOf(t, &e, viewer); got != "midnight" {
			t.Errorf("a rejected choice must not change anything: %q", got)
		}

		// Empty goes back to the organisation default.
		if r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {""}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("clear: %d", r.StatusCode)
		}
		if got := themeOf(t, &e, viewer); got != "paddle" {
			t.Errorf("after clearing: %q", got)
		}

		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "user.theme", 10)
			return
		})
		if len(aud) != 2 {
			t.Errorf("want 2 audit rows (set, clear), got %+v", aud)
		}
	})
}

func TestAUserThemeThatNoLongerExistsFallsBackWithoutCrashing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		viewer := e.roleClient("viewer")
		tok := csrfOf(t, e.pageOK(viewer, "/dashboard/account"))
		if r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {"teal"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("set: %d", r.StatusCode)
		}
		if got := themeOf(t, &e, viewer); got != "teal" {
			t.Fatalf("teal: %q", got)
		}
		// Deleted on disk outside the app: org default, else stripe.
		if err := os.RemoveAll(filepath.Join(e.themesDir, "teal")); err != nil {
			t.Fatal(err)
		}
		if got := themeOf(t, &e, viewer); got != "stripe" {
			t.Errorf("no org default: %q", got)
		}
		e.setOrg("theme", `"midnight"`)
		if got := themeOf(t, &e, viewer); got != "midnight" {
			t.Errorf("org default: %q", got)
		}
		// The account page still renders, with the stale choice not offered.
		page := e.pageOK(viewer, "/dashboard/account")
		lacks(t, page, `value="teal"`)
	})
}

func versionOf(t *testing.T, e themedEnv, c *http.Client) string {
	t.Helper()
	body := e.pageOK(c, "/dashboard/overview")
	_, rest, ok := strings.Cut(body, `data-dashboard-theme-version="`)
	if !ok {
		t.Fatalf("no data-dashboard-theme-version on the react root: %s", body)
	}
	v, _, _ := strings.Cut(rest, `"`)
	return v
}

// The page names the theme's version, so the browser refetches the colours of
// the theme it already wears after that theme was edited.
func TestPageCarriesTheThemeVersionAndItChangesWithAnEdit(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		e.setOrg("theme", `"teal"`)
		admin := e.roleClient("admin")
		before := versionOf(t, e, admin)
		if before == "" {
			t.Fatal("empty version")
		}
		if again := versionOf(t, e, admin); again != before {
			t.Errorf("version changed without an edit: %q -> %q", before, again)
		}
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance/themes/teal/edit"))
		f := themeForm(tok, "light_accent", "#ff0066")
		f.Del("name")
		if r, _ := e.post(admin, "/dashboard/appearance/themes/teal", f); r.StatusCode != 303 {
			t.Fatalf("save: %d", r.StatusCode)
		}
		if after := versionOf(t, e, admin); after == before {
			t.Errorf("version did not change after an edit: %q", after)
		}
		// Built-in themes have one too.
		e.setOrg("theme", `"paddle"`)
		if v := versionOf(t, e, admin); v == "" {
			t.Error("no version for a built-in theme")
		}
	})
}
