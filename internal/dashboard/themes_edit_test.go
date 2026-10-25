package dashboard

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// themeForm is a valid create/save form; tests override single fields.
func themeForm(tok string, kv ...string) url.Values {
	f := url.Values{
		"csrf": {tok}, "name": {"teal"}, "version": {"1.0.0"}, "description": {"Teal"}, "scheme": {""},
		"light_accent": {"#0d7d68"},
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "-" {
			f.Del(kv[i])
		} else {
			f.Set(kv[i], kv[i+1])
		}
	}
	return f
}

func readManifest(t *testing.T, e themedEnv, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.themesDir, name, "theme.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return m
}

func auditRows(t *testing.T, db *store.DB, action string) []store.AuditRow {
	t.Helper()
	var rows []store.AuditRow
	if err := db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = store.ListAudit(ctx, q, store.DefaultOrgID, action, 20)
		return
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCardsOfferEditingActionsToAdminsOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		body := e.pageOK(e.roleClient("admin"), "/dashboard/appearance")
		has(t, body, `href="/dashboard/appearance/themes/new"`)
		for _, name := range []string{"stripe", "teal"} {
			has(t, card(t, body, name), `href="/dashboard/appearance/themes/new?from=`+name+`"`)
		}
		// Edit: custom themes only.
		has(t, card(t, body, "teal"), `href="/dashboard/appearance/themes/teal/edit"`)
		lacks(t, card(t, body, "stripe"), `/themes/stripe/edit`, `/themes/stripe/rename`, `/themes/stripe/delete`)

		view := e.pageOK(e.roleClient("viewer"), "/dashboard/appearance")
		lacks(t, view, `/appearance/themes/new`, `/edit"`, `/rename`, `/delete`, `/themes/import`)
	})
}

func TestNewThemeFormIsPrefilledFromAnotherTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		body := e.pageOK(admin, "/dashboard/appearance/themes/new")
		has(t, body, `action="/dashboard/appearance/themes"`, `name="name"`, `name="version"`, `type="color"`, `name="light_accent"`,
			`name="dark_enabled"`, `name="scheme"`, `name="csrf"`)
		body = e.pageOK(admin, "/dashboard/appearance/themes/new?from=paddle")
		has(t, body, `value="#0d7d68"`, `value="#121417"`, `value="#e8e8e3"`, `name="from" value="paddle"`)
		if r, _ := e.get(admin, "/dashboard/appearance/themes/new?from=nope"); r.StatusCode != 404 {
			t.Errorf("unknown source: %d", r.StatusCode)
		}
		if r, _ := e.get(e.roleClient("viewer"), "/dashboard/appearance/themes/new"); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
	})
}

func TestAdminCreatesATheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance/themes/new"))
		form := themeForm(tok,
			"light_background", "#fbfaf7", "set_light_background", "1",
			"dark_enabled", "1", "dark_accent", "#34c9a9", "dark_background", "#14161a", "set_dark_background", "1",
			"scheme", "dark")
		r, _ := e.post(admin, "/dashboard/appearance/themes", form)
		if r.StatusCode != 303 {
			t.Fatalf("create: %d", r.StatusCode)
		}
		m := readManifest(t, e, "teal")
		dash := m["dashboard"].(map[string]any)
		seeds := dash["seeds"].(map[string]any)
		dark := dash["dark_seeds"].(map[string]any)
		if m["name"] != "teal" || m["version"] != "1.0.0" || seeds["accent"] != "#0d7d68" || seeds["background"] != "#fbfaf7" ||
			dark["accent"] != "#34c9a9" || dash["scheme"] != "dark" {
			t.Errorf("manifest: %v", m)
		}
		if _, set := seeds["sidebar"]; set {
			t.Errorf("an unchecked optional seed was saved: %v", seeds)
		}
		// The next request already shows it: no waiting for a reload.
		body := e.pageOK(admin, "/dashboard/appearance")
		has(t, card(t, body, "teal"), `data-kind="custom"`, `#34c9a9`)
		if rows := auditRows(t, db, "theme.create"); len(rows) != 1 || rows[0].Target != "teal" {
			t.Errorf("audit: %+v", rows)
		}
		// Usable right away as a choice.
		has(t, e.pageOK(admin, "/dashboard/account"), `value="teal"`)
	})
}

func TestCreateRefusesBadInputAndWritesNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("taken", tealTheme, nil)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance/themes/new"))
		cases := []struct {
			what string
			form url.Values
			want string
		}{
			{"bundled stripe", themeForm(tok, "name", "stripe"), "built-in"},
			{"bundled default", themeForm(tok, "name", "default"), "built-in"},
			{"bundled minimal-dark", themeForm(tok, "name", "minimal-dark"), "built-in"},
			{"existing", themeForm(tok, "name", "taken"), "already exists"},
			{"traversal", themeForm(tok, "name", "../evil"), "lower-case"},
			{"slash", themeForm(tok, "name", "a/b"), "lower-case"},
			{"upper case", themeForm(tok, "name", "Bad Name"), "lower-case"},
			{"empty name", themeForm(tok, "name", ""), "lower-case"},
			{"bad hex", themeForm(tok, "light_accent", "red"), "#rrggbb"},
			{"no accent", themeForm(tok, "light_accent", "-"), "accent"},
			{"bad version", themeForm(tok, "version", "!!"), "version"},
			{"bad scheme", themeForm(tok, "scheme", "sepia"), "scheme"},
			{"long description", themeForm(tok, "description", strings.Repeat("x", 201)), "description"},
			{"dark on, no dark accent", themeForm(tok, "dark_enabled", "1"), "accent"},
			{"bad optional seed", themeForm(tok, "light_text", "nope", "set_light_text", "1"), "#rrggbb"},
		}
		for _, c := range cases {
			r, body := e.post(admin, "/dashboard/appearance/themes", c.form)
			if r.StatusCode != 400 {
				t.Errorf("%s: %d, want 400", c.what, r.StatusCode)
				continue
			}
			has(t, body, c.want, `name="light_accent"`) // the form is shown again, with the message
		}
		entries, _ := os.ReadDir(e.themesDir)
		if len(entries) != 1 {
			t.Errorf("only the pre-existing theme should be on disk: %v", entries)
		}
		if r, _ := e.post(admin, "/dashboard/appearance/themes", themeForm("")); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
		op := e.roleClient("operator")
		if r, _ := e.post(op, "/dashboard/appearance/themes", themeForm(csrfOf(t, e.pageOK(op, "/dashboard/overview")))); r.StatusCode != 403 {
			t.Errorf("operator: %d", r.StatusCode)
		}
	})
}

func TestDuplicateKeepsCustomerPageFilesButDropsOverrides(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("src", `{"name":"src","version":"1.0.0","font_stack":"Georgia, serif","tokens":{"--radius":"2px"},"dashboard":{"seeds":{"accent":"#0d7d68"},"overrides":{"light":{"--border":"#112233"}}}}`,
			map[string]string{"css/a.css": ".a{color:red}"})
		admin := e.roleClient("admin")
		page := e.pageOK(admin, "/dashboard/appearance/themes/new?from=src")
		has(t, page, `value="#0d7d68"`)
		tok := csrfOf(t, page)
		r, _ := e.post(admin, "/dashboard/appearance/themes", themeForm(tok, "name", "copy", "from", "src"))
		if r.StatusCode != 303 {
			t.Fatalf("duplicate: %d", r.StatusCode)
		}
		m := readManifest(t, e, "copy")
		if m["font_stack"] != "Georgia, serif" || m["tokens"].(map[string]any)["--radius"] != "2px" {
			t.Errorf("customer-page fields lost: %v", m)
		}
		if _, has := m["dashboard"].(map[string]any)["overrides"]; has {
			t.Errorf("overrides were copied: %v", m["dashboard"])
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "copy", "css", "a.css")); err != nil {
			t.Errorf("css not copied: %v", err)
		}
		// A built-in theme can be duplicated as well.
		if r, _ := e.post(admin, "/dashboard/appearance/themes", themeForm(tok, "name", "my-paddle", "from", "paddle")); r.StatusCode != 303 {
			t.Fatalf("duplicate paddle: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/appearance/themes", themeForm(tok, "name", "x1", "from", "nope")); r.StatusCode != 400 {
			t.Errorf("unknown source: %d", r.StatusCode)
		}
	})
}

func TestAdminEditsACustomThemeAndEverythingElseIsKept(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", `{"name":"mine","version":"1.0.0","tokens":{"--radius":"2px"},"dashboard":{"seeds":{"accent":"#0d7d68","sidebar":"#121417"},"overrides":{"light":{"--border":"#112233"}}}}`,
			map[string]string{"css/a.css": ".a{}"})
		admin := e.roleClient("admin")
		page := e.pageOK(admin, "/dashboard/appearance/themes/mine/edit")
		has(t, page, `action="/dashboard/appearance/themes/mine"`, `value="#0d7d68"`, `value="#121417"`, `name="set_light_sidebar" value="1" checked`)
		lacks(t, page, `name="name"`) // a name is changed by Rename, not here
		// The lazy editor previews the theme with its hand-set colours too.
		has(t, page, `data-theme-editor`, `data-overrides=`, `#112233`)
		tok := csrfOf(t, page)

		f := themeForm(tok, "version", "1.1.0", "light_accent", "#ff0066", "light_sidebar", "#121417", "set_light_sidebar", "1")
		f.Del("name")
		r, _ := e.post(admin, "/dashboard/appearance/themes/mine", f)
		if r.StatusCode != 303 {
			t.Fatalf("save: %d", r.StatusCode)
		}
		m := readManifest(t, e, "mine")
		dash := m["dashboard"].(map[string]any)
		if m["version"] != "1.1.0" || dash["seeds"].(map[string]any)["accent"] != "#ff0066" ||
			m["tokens"].(map[string]any)["--radius"] != "2px" ||
			dash["overrides"].(map[string]any)["light"].(map[string]any)["--border"] != "#112233" {
			t.Errorf("after save: %v", m)
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "mine", "css", "a.css")); err != nil {
			t.Errorf("css lost: %v", err)
		}
		has(t, card(t, e.pageOK(admin, "/dashboard/appearance"), "mine"), `#ff0066`, `1.1.0`)
		if rows := auditRows(t, db, "theme.update"); len(rows) != 1 || rows[0].Target != "mine" {
			t.Errorf("audit: %+v", rows)
		}

		// Unchecking an optional seed removes it; an invalid save changes nothing.
		f = themeForm(tok, "light_accent", "#ff0066")
		f.Del("name")
		if r, _ := e.post(admin, "/dashboard/appearance/themes/mine", f); r.StatusCode != 303 {
			t.Fatalf("save 2: %d", r.StatusCode)
		}
		if _, set := readManifest(t, e, "mine")["dashboard"].(map[string]any)["seeds"].(map[string]any)["sidebar"]; set {
			t.Error("the unchecked sidebar seed stayed")
		}
		before := readManifest(t, e, "mine")
		f = themeForm(tok, "light_accent", "bad")
		f.Del("name")
		if r, body := e.post(admin, "/dashboard/appearance/themes/mine", f); r.StatusCode != 400 {
			t.Errorf("invalid save: %d", r.StatusCode)
		} else {
			has(t, body, `#rrggbb`)
		}
		if after := readManifest(t, e, "mine"); !jsonEqual(before, after) {
			t.Errorf("an invalid save changed the theme:\n%v\n%v", before, after)
		}
	})
}

// tealAs is the teal theme under another name (the name inside must match its directory).
func tealAs(name string) string {
	return strings.Replace(tealTheme, `"teal"`, `"`+name+`"`, 1)
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestBuiltInThemesAreReadOnlyAndNamesCannotEscape(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		for _, name := range []string{"stripe", "paddle", "midnight", "default", "minimal-dark"} {
			if r, _ := e.get(admin, "/dashboard/appearance/themes/"+name+"/edit"); r.StatusCode != 403 {
				t.Errorf("edit %s: %d, want 403", name, r.StatusCode)
			}
			f := themeForm(tok)
			f.Del("name")
			if r, _ := e.post(admin, "/dashboard/appearance/themes/"+name, f); r.StatusCode != 403 {
				t.Errorf("save %s: %d, want 403", name, r.StatusCode)
			}
		}
		for _, p := range []string{"..", "%2e%2e", "a%2fb", "Bad", "nope"} {
			// Refused outright, or the mux first redirects to the cleaned path; never served.
			if r, _ := e.get(admin, "/dashboard/appearance/themes/"+p+"/edit"); r.StatusCode == 200 {
				t.Errorf("edit %q: %d", p, r.StatusCode)
			}
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "stripe")); err == nil {
			t.Error("a directory shadowing stripe was written")
		}
	})
}

func TestThemeWritesNeedARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", tealAs("mine"), nil)
		c := staleAdmin(t, e.env)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))
		for path, form := range map[string]url.Values{
			"/dashboard/appearance/themes":      themeForm(tok, "name", "fresh"),
			"/dashboard/appearance/themes/mine": themeForm(tok),
		} {
			r, _ := e.post(c, path, form)
			wantReauth(t, "POST "+path, r)
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "fresh")); err == nil {
			t.Error("a stale sign-in created a theme")
		}
	})
}

func TestCardsOfferRenameAndDeleteForCustomThemesOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		body := e.pageOK(e.roleClient("admin"), "/dashboard/appearance")
		has(t, card(t, body, "teal"), `action="/dashboard/appearance/themes/teal/rename"`, `name="new_name"`, `action="/dashboard/appearance/themes/teal/delete"`)
		lacks(t, card(t, body, "stripe"), `/rename`, `/delete`)
		lacks(t, e.pageOK(e.roleClient("viewer"), "/dashboard/appearance"), `/rename`, `/delete`)
	})
}

func TestRenameMakesEveryReferenceFollowAtOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", tealAs("mine"), nil)
		e.setOrg("theme", `"mine"`)
		admin, viewer := e.roleClient("admin"), e.roleClient("viewer")
		// Prime the settings cache: the rename must not leave the old value in it.
		if v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "theme"); string(v.Raw) != `"mine"` {
			t.Fatalf("setting = %s", v.Raw)
		}
		vtok := csrfOf(t, e.pageOK(viewer, "/dashboard/account"))
		if r, _ := e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {"mine"}, "csrf": {vtok}}); r.StatusCode != 303 {
			t.Fatalf("viewer chooses: %d", r.StatusCode)
		}
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		r, _ := e.post(admin, "/dashboard/appearance/themes/mine/rename", url.Values{"csrf": {tok}, "new_name": {"ours"}})
		if r.StatusCode != 303 {
			t.Fatalf("rename: %d", r.StatusCode)
		}
		if v, _ := e.set.ResolveOrg(ctx, store.DefaultOrgID, "theme"); string(v.Raw) != `"ours"` {
			t.Errorf("the org default did not follow the rename: %s", v.Raw)
		}
		if got := themeOf(t, &e, viewer); got != "ours" {
			t.Errorf("the user's choice did not follow: %q", got)
		}
		if got := themeOf(t, &e, admin); got != "ours" {
			t.Errorf("the org default is not worn: %q", got)
		}
		body := e.pageOK(admin, "/dashboard/appearance")
		has(t, card(t, body, "ours"), `data-default="true"`, `data-users="1"`)
		lacks(t, body, `data-theme-card="mine"`)
		if m := readManifest(t, e, "ours"); m["name"] != "ours" {
			t.Errorf("manifest name: %v", m["name"])
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "mine")); err == nil {
			t.Error("old directory remains")
		}
		rows := auditRows(t, db, "theme.rename")
		if len(rows) != 1 || rows[0].Target != "mine" {
			t.Errorf("audit: %+v", rows)
		}
	})
}

func TestRenameRefusals(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", tealAs("mine"), nil)
		e.writeTheme("other", tealAs("other"), nil)
		alpha := e.seedApp("alpha")
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		rename := func(name, to string) (int, string) {
			r, body := e.post(admin, "/dashboard/appearance/themes/"+name+"/rename", url.Values{"csrf": {tok}, "new_name": {to}})
			return r.StatusCode, body
		}
		for what, to := range map[string]string{"bundled name": "stripe", "existing": "other", "traversal": "../x", "upper case": "Bad", "empty": ""} {
			if code, _ := rename("mine", to); code != 400 {
				t.Errorf("%s: %d, want 400", what, code)
			}
		}
		if code, _ := rename("stripe", "mine2"); code != 403 {
			t.Errorf("bundled source: %d", code)
		}
		if code, _ := rename("nope", "mine2"); code != 404 {
			t.Errorf("missing: %d", code)
		}
		// An app's customer pages use it: blocked, with the apps named.
		atok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		if r, _ := e.post(admin, "/dashboard/appearance/apps/"+alpha.ID, url.Values{"csrf": {atok}, "theme": {"mine"}, "language": {""}}); r.StatusCode != 303 {
			t.Fatalf("app theme: %d", r.StatusCode)
		}
		code, body := rename("mine", "fresh")
		if code != 400 {
			t.Fatalf("rename while an app uses it: %d", code)
		}
		has(t, body, alpha.Name, "customer pages")
		if _, err := os.Stat(filepath.Join(e.themesDir, "mine")); err != nil {
			t.Error("a refused rename moved the theme")
		}
		if len(auditRows(t, db, "theme.rename")) != 0 {
			t.Error("a refused rename was audited as done")
		}
	})
}

func TestDeleteIsRefusedWhileInUseAndListsWho(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", tealAs("mine"), nil)
		e.writeTheme("spare", tealAs("spare"), nil)
		admin, viewer := e.roleClient("admin"), e.roleClient("viewer")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		del := func(name string) (int, string) {
			r, body := e.post(admin, "/dashboard/appearance/themes/"+name+"/delete", url.Values{"csrf": {tok}})
			return r.StatusCode, body
		}
		e.setOrg("theme", `"mine"`)
		code, body := del("mine")
		if code != 400 {
			t.Fatalf("delete the org default: %d", code)
		}
		has(t, body, "organisation default")
		e.setOrg("theme", `"default"`)

		vtok := csrfOf(t, e.pageOK(viewer, "/dashboard/account"))
		_, _ = e.post(viewer, "/dashboard/account/theme", url.Values{"theme": {"mine"}, "csrf": {vtok}})
		code, body = del("mine")
		if code != 400 {
			t.Fatalf("delete while a user chose it: %d", code)
		}
		has(t, body, "viewer@") // the people are named
		if _, err := os.Stat(filepath.Join(e.themesDir, "mine")); err != nil {
			t.Fatal("a refused delete removed the theme")
		}

		// Unused: gone at once, from disk and from the page, and audited.
		code, _ = del("spare")
		if code != 303 {
			t.Fatalf("delete unused: %d", code)
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "spare")); err == nil {
			t.Error("directory remains")
		}
		lacks(t, e.pageOK(admin, "/dashboard/appearance"), `data-theme-card="spare"`)
		if rows := auditRows(t, db, "theme.delete"); len(rows) != 1 || rows[0].Target != "spare" {
			t.Errorf("audit: %+v", rows)
		}
		if code, _ := del("stripe"); code != 403 {
			t.Errorf("bundled: %d", code)
		}
		if code, _ := del("nope"); code != 404 {
			t.Errorf("missing: %d", code)
		}
	})
}

func TestRenameAndDeleteNeedARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", tealAs("mine"), nil)
		c := staleAdmin(t, e.env)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))
		for path, form := range map[string]url.Values{
			"/dashboard/appearance/themes/mine/rename": {"csrf": {tok}, "new_name": {"other"}},
			"/dashboard/appearance/themes/mine/delete": {"csrf": {tok}},
		} {
			r, _ := e.post(c, path, form)
			wantReauth(t, "POST "+path, r)
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "mine")); err != nil {
			t.Error("a stale sign-in changed the theme")
		}
	})
}
