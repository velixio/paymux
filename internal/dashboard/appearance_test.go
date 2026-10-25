package dashboard

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/web"
)

// themedEnv is an env whose renderer knows the bundled themes plus the ones
// in a temporary themes directory. alerts counts "unusable theme" alerts.
type themedEnv struct {
	*env
	themesDir string
	alerts    *atomic.Int32
}

func newThemedEnv(t *testing.T, db *store.DB) themedEnv {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "themes")
	var alerts atomic.Int32
	themes := web.NewThemes(web.Bundled(), dir)
	rend, err := web.NewThemedRenderer(web.ThemedOptions{Base: web.Embedded(), Themes: themes, Recheck: -1,
		Alert: func(string, error) { alerts.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, db, func(o *Options) { o.Renderer, o.Themes = rend, themes })
	return themedEnv{env: e, themesDir: dir, alerts: &alerts}
}

func (e themedEnv) writeTheme(name, manifest string, extra map[string]string) {
	e.t.Helper()
	files := map[string]string{"theme.json": manifest}
	for k, v := range extra {
		files[k] = v
	}
	for p, c := range files {
		full := filepath.Join(e.themesDir, name, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

const pinkTheme = `{"name":"pink","version":"2.1.0","description":"Hot pink accents","tokens":{"--color-accent":"#ff0066"}}`

func (e themedEnv) setOrg(key, raw string) {
	e.t.Helper()
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, json.RawMessage(raw)); err != nil {
		e.t.Fatal(err)
	}
}

func TestDashboardWearsTheOrgTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		has(t, e.pageOK(admin, "/dashboard/overview"), `href="/theme/default/theme.css`)
		if r, body := e.get(e.client(), "/dashboard/login"); r.StatusCode != 200 || !strings.Contains(body, `/theme/default/theme.css`) {
			t.Fatalf("login page: %d %s", r.StatusCode, body)
		}
		e.setOrg("theme", `"minimal-dark"`)
		body := e.pageOK(admin, "/dashboard/overview")
		has(t, body, `href="/theme/minimal-dark/theme.css`)
		lacks(t, body, `/theme/default/`)
		// A theme that is not usable never breaks the dashboard.
		e.setOrg("theme", `"gone"`)
		body = e.pageOK(admin, "/dashboard/overview")
		lacks(t, body, "/theme/")
		has(t, body, "/assets/css/dash.css")
		if e.alerts.Load() == 0 {
			t.Error("an unusable theme should raise an alert")
		}
	})
}

func TestAppearancePageListsThemesAndTheirProblems(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("pink", pinkTheme, nil)
		e.writeTheme("oops", `{"name":"oops","version":"1","tokens":{"--x":"red; } body{display:none"}}`, nil)
		alpha := e.seedApp("alpha")
		e.otherOrg()

		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/appearance")
		has(t, body, "default", "minimal-dark", "pink", "2.1.0", "Hot pink accents", "oops", "not allowed", alpha.Name,
			`/dashboard/appearance/preview/pink`, `/dashboard/appearance/preview/minimal-dark`)
		lacks(t, body, "Foreign App", "app_foreign")
		lacks(t, body, `action="/dashboard/appearance`) // read-only for viewers

		admin := e.roleClient("admin")
		body = e.pageOK(admin, "/dashboard/appearance")
		has(t, body, `action="/dashboard/appearance/apps/`+alpha.ID+`"`, `name="theme"`, `name="language"`)
		// Themes that cannot be used are not offered as choices.
		opts := regexpFind(t, `(?s)(<select[^>]*name="theme"[^>]*>.*?</select>)`, body)
		has(t, opts, `value="pink"`, `value="minimal-dark"`)
		lacks(t, opts, `value="oops"`)
	})
}

func TestAdminChoosesTheDashboardTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("pink", pinkTheme, nil)
		e.writeTheme("oops", `{"name":"oops"}`, nil)
		admin, op := e.roleClient("admin"), e.roleClient("operator")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))

		r, _ := e.post(admin, "/dashboard/appearance/theme", url.Values{"theme": {"pink"}, "csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("set theme: %d", r.StatusCode)
		}
		// Applied at once, no restart.
		has(t, e.pageOK(admin, "/dashboard/overview"), `/theme/pink/theme.css`)

		for name, c := range map[string]string{"unknown": "nope", "invalid": "oops", "traversal": "../etc", "empty": "", "bad name": "Pink!"} {
			if r, _ := e.post(admin, "/dashboard/appearance/theme", url.Values{"theme": {c}, "csrf": {tok}}); r.StatusCode != 400 {
				t.Errorf("%s: %d, want 400", name, r.StatusCode)
			}
		}
		if r, _ := e.post(admin, "/dashboard/appearance/theme", url.Values{"theme": {"minimal-dark"}}); r.StatusCode != 403 {
			t.Errorf("missing csrf: %d", r.StatusCode)
		}
		opTok := csrfOf(t, e.pageOK(op, "/dashboard/appearance"))
		if r, _ := e.post(op, "/dashboard/appearance/theme", url.Values{"theme": {"minimal-dark"}, "csrf": {opTok}}); r.StatusCode != 403 {
			t.Errorf("operators may not change settings: %d", r.StatusCode)
		}
		v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "theme")
		if string(v.Raw) != `"pink"` {
			t.Errorf("theme = %s", v.Raw)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "setting.update", 10)
			return
		})
		if len(aud) != 1 || !strings.HasSuffix(aud[0].Target, "/theme") {
			t.Errorf("audit: %+v", aud)
		}
	})
}

func TestAdminChoosesAnAppsThemeAndLanguage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("pink", pinkTheme, nil)
		alpha := e.seedApp("alpha")
		e.otherOrg()
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		path := "/dashboard/appearance/apps/" + alpha.ID

		if r, _ := e.post(admin, path, url.Values{"theme": {"pink"}, "language": {"hi"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("set: %d", r.StatusCode)
		}
		for key, want := range map[string]string{"theme": `"pink"`, "customer_language": `"hi"`} {
			v, err := e.set.Resolve(ctx, alpha.ID, key)
			if err != nil || string(v.Raw) != want || v.Source != "app" {
				t.Errorf("%s = %+v %v", key, v, err)
			}
		}
		// "inherit" (empty) removes the app's own value.
		if r, _ := e.post(admin, path, url.Values{"theme": {""}, "language": {""}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("reset: %d", r.StatusCode)
		}
		for _, key := range []string{"theme", "customer_language"} {
			if v, _ := e.set.Resolve(ctx, alpha.ID, key); v.Source == "app" {
				t.Errorf("%s still set on the app: %+v", key, v)
			}
		}
		for name, form := range map[string]url.Values{
			"unknown theme":    {"theme": {"nope"}, "language": {""}},
			"unknown language": {"theme": {""}, "language": {"zz"}},
			"path language":    {"theme": {""}, "language": {"../hi"}},
		} {
			form.Set("csrf", tok)
			if r, _ := e.post(admin, path, form); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if r, _ := e.post(admin, "/dashboard/appearance/apps/app_foreign", url.Values{"theme": {"pink"}, "language": {""}, "csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("another org's app: %d, want 404", r.StatusCode)
		}
		if v, _ := e.set.Resolve(ctx, "app_foreign", "theme"); v.Source == "app" {
			t.Error("a foreign app was changed")
		}
	})
}

func TestThemePreviewRendersASampleInThatTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("pink", pinkTheme, nil)
		e.writeTheme("broken", pinkTheme[:30], nil)
		viewer := e.roleClient("viewer")
		body := e.pageOK(viewer, "/dashboard/appearance/preview/pink")
		has(t, body, `/theme/pink/theme.css`, "pink", "2.1.0", `class="btn"`, `class="field"`)
		has(t, e.pageOK(viewer, "/dashboard/appearance/preview/minimal-dark"), `/theme/minimal-dark/theme.css`)
		// A broken theme previews as the fallback look and says why.
		body = e.pageOK(viewer, "/dashboard/appearance/preview/broken")
		lacks(t, body, "/theme/broken/")
		has(t, body, "cannot be used")
		for _, p := range []string{"nope", "..%2f..%2fetc", "Bad%20Name"} {
			if r, _ := e.get(viewer, "/dashboard/appearance/preview/"+p); r.StatusCode != 404 {
				t.Errorf("preview %s: %d", p, r.StatusCode)
			}
		}
	})
}
