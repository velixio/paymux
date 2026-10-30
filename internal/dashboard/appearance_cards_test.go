package dashboard

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// card returns the markup of one theme card, or fails.
func card(t *testing.T, body, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)<li class="theme-card"[^>]*data-theme-card="` + regexp.QuoteMeta(name) + `".*?</li>`)
	m := re.FindString(body)
	if m == "" {
		t.Fatalf("no card for %q in: %s", name, body)
	}
	return m
}

func TestAppearanceShowsThemeCards(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		e.writeTheme("pink", pinkTheme, nil) // customer-page theme: no dashboard block
		e.writeTheme("oops", `{"name":"oops","version":"1","tokens":{"--x":"red; } body{display:none"}}`, nil)
		e.setOrg("theme", `"paddle"`)
		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/appearance")

		for _, name := range []string{"stripe", "paddle", "midnight", "teal"} {
			c := card(t, body, name)
			has(t, c, `data-theme-preview`, `data-theme-spec=`, `/dashboard/appearance/preview/`+name)
		}
		// The preview spec carries the seeds for the client to derive from.
		has(t, card(t, body, "paddle"), `#0d7d68`, `#121417`)

		// Built-in or custom, version, and who is the default.
		has(t, card(t, body, "stripe"), `data-kind="builtin"`, `Built-in`, `data-default="false"`)
		has(t, card(t, body, "teal"), `data-kind="custom"`, `Custom`, `1.0.0`)
		has(t, card(t, body, "paddle"), `data-default="true"`, `Default`)
		lacks(t, card(t, body, "stripe"), `>Default<`)

		// Customer-page-only themes are listed, but have no dashboard preview.
		lacks(t, card(t, body, "pink"), `data-theme-preview`)
		has(t, card(t, body, "pink"), `Hot pink accents`)
		has(t, card(t, body, "oops"), `not allowed`)
		lacks(t, card(t, body, "oops"), `data-theme-preview`)

		// Viewers see the cards without any action.
		lacks(t, body, `action="/dashboard/appearance/theme"`, `Set as default`)
	})
}

func TestAppearanceCardsCountWhoChoseTheTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin, viewer := e.roleClient("admin"), e.roleClient("viewer")
		tok := csrfOf(t, e.pageOK(viewer, "/dashboard/account"))
		if r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {"midnight"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("choose: %d", r.StatusCode)
		}
		body := e.pageOK(admin, "/dashboard/appearance")
		has(t, card(t, body, "midnight"), `data-users="1"`)
		has(t, card(t, body, "paddle"), `data-users="0"`)
	})
}

func TestAdminSetsTheDefaultFromACard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.setOrg("theme", `"paddle"`)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/appearance")
		has(t, card(t, body, "midnight"), `action="/dashboard/appearance/theme"`, `name="theme" value="midnight"`, `Set as default`)
		// The default needs no button.
		lacks(t, card(t, body, "paddle"), `Set as default`)

		tok := csrfOf(t, body)
		if r, _ := e.post(admin, "/dashboard/appearance/theme", url.Values{"theme": {"midnight"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("set default: %d", r.StatusCode)
		}
		body = e.pageOK(admin, "/dashboard/appearance")
		has(t, card(t, body, "midnight"), `data-default="true"`)
		has(t, card(t, body, "paddle"), `data-default="false"`)
		// Everyone without a choice of their own wears it.
		if got := themeOf(t, &e, e.roleClient("operator")); got != "midnight" {
			t.Errorf("operator wears %q", got)
		}
		if v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "theme"); !strings.Contains(string(v.Raw), "midnight") {
			t.Errorf("setting = %s", v.Raw)
		}
	})
}
