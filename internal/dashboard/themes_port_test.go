package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const richTheme = `{"name":"rich","version":"2.0.0","description":"Has everything","font_stack":"Georgia, serif","tokens":{"--radius":"2px","--color-accent":"#ff0066"},"dashboard":{"seeds":{"accent":"#0d7d68","sidebar":"#121417"},"dark_seeds":{"accent":"#34c9a9"},"scheme":"dark","overrides":{"light":{"--border":"#112233"}}}}`

// part is one file of a multipart import.
type part struct{ field, name, data string }

func (e themedEnv) importRaw(c *http.Client, fields map[string]string, files ...part) (*http.Response, string) {
	e.t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for _, f := range files {
		fw, _ := w.CreateFormFile(f.field, f.name)
		_, _ = fw.Write([]byte(f.data))
	}
	_ = w.Close()
	resp, err := c.Post(e.srv.URL+"/dashboard/appearance/themes/import", w.FormDataContentType(), &buf)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func onDisk(t *testing.T, e themedEnv) []string {
	t.Helper()
	entries, _ := os.ReadDir(e.themesDir)
	var out []string
	for _, x := range entries {
		out = append(out, x.Name())
	}
	return out
}

func TestExportDownloadsTheThemeAsIs(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("rich", richTheme, map[string]string{"css/a.css": ".a{}"})
		viewer := e.roleClient("viewer") // whoever can see the page can export
		r, body := e.get(viewer, "/dashboard/appearance/themes/rich/export.json")
		if r.StatusCode != 200 {
			t.Fatalf("export: %d %s", r.StatusCode, body)
		}
		if cd := r.Header.Get("Content-Disposition"); cd != `attachment; filename="rich.theme.json"` {
			t.Errorf("content-disposition %q", cd)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content type %q", ct)
		}
		if r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("no nosniff")
		}
		var got, want map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		_ = json.Unmarshal([]byte(richTheme), &want)
		if !jsonEqual(got, want) {
			t.Errorf("export differs from the theme:\n%v\n%v", got, want)
		}
		// Only theme.json: no stylesheet or asset travels.
		lacks(t, body, ".a{}")

		// A built-in theme exports too; unknown, unusable and odd names do not.
		if r, b := e.get(viewer, "/dashboard/appearance/themes/paddle/export.json"); r.StatusCode != 200 || !strings.Contains(b, `"#0d7d68"`) {
			t.Errorf("paddle: %d", r.StatusCode)
		}
		for _, p := range []string{"nope", "Bad", "..%2f..%2fetc", "a%2fb"} {
			if r, _ := e.get(viewer, "/dashboard/appearance/themes/"+p+"/export.json"); r.StatusCode == 200 {
				t.Errorf("%q exported", p)
			}
		}
		// Signed-out visitors are sent to log in.
		if r, _ := e.get(e.client(), "/dashboard/appearance/themes/rich/export.json"); r.StatusCode == 200 {
			t.Errorf("signed out: %d", r.StatusCode)
		}
	})
}

func TestCardsOfferExportAndAdminsAnImportForm(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("teal", tealTheme, nil)
		viewer := e.pageOK(e.roleClient("viewer"), "/dashboard/appearance")
		for _, n := range []string{"stripe", "teal"} {
			has(t, card(t, viewer, n), `/dashboard/appearance/themes/`+n+`/export.json`)
		}
		lacks(t, viewer, `/appearance/themes/import`)
		admin := e.pageOK(e.roleClient("admin"), "/dashboard/appearance")
		has(t, admin, `action="/dashboard/appearance/themes/import"`, `enctype="multipart/form-data"`, `type="file"`, `name="theme"`, `name="name"`)
	})
}

func TestImportCreatesTheThemeAndRoundTrips(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("rich", richTheme, nil)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		_, exported := e.get(admin, "/dashboard/appearance/themes/rich/export.json")

		// Same install: the name is taken, so a new one is given in the form.
		r, _ := e.importRaw(admin, map[string]string{"csrf": tok, "name": "rich-copy"}, part{"theme", "whatever.json", exported})
		if r.StatusCode != 303 {
			t.Fatalf("import: %d", r.StatusCode)
		}
		var a, b map[string]any
		_ = json.Unmarshal([]byte(richTheme), &a)
		b = readManifest(t, e, "rich-copy")
		a["name"] = "rich-copy"
		if !jsonEqual(a, b) {
			t.Errorf("round trip changed the theme:\n%v\n%v", a, b)
		}
		// Visible at once, and the filename did not matter.
		has(t, card(t, e.pageOK(admin, "/dashboard/appearance"), "rich-copy"), `data-kind="custom"`, `2.0.0`)
		if rows := auditRows(t, db, "theme.import"); len(rows) != 1 || rows[0].Target != "rich-copy" {
			t.Errorf("audit: %+v", rows)
		}
		// Without a name in the form, the file's own name is used.
		other := strings.Replace(richTheme, `"rich"`, `"fromfile"`, 1)
		if r, _ := e.importRaw(admin, map[string]string{"csrf": tok}, part{"theme", "x.json", other}); r.StatusCode != 303 {
			t.Fatalf("import by file name: %d", r.StatusCode)
		}
		if _, err := os.Stat(filepath.Join(e.themesDir, "fromfile", "theme.json")); err != nil {
			t.Error(err)
		}
	})
}

func TestImportAsksForANewNameOnACollisionAndKeepsTheFile(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("rich", richTheme, nil)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		for what, name := range map[string]string{"bundled": "stripe", "existing": "rich"} {
			file := strings.Replace(richTheme, `"rich"`, `"`+name+`"`, 1)
			r, body := e.importRaw(admin, map[string]string{"csrf": tok}, part{"theme", "t.json", file})
			if r.StatusCode != 400 {
				t.Fatalf("%s: %d", what, r.StatusCode)
			}
			// The import form is shown again, with the reason and a name field.
			has(t, body, `action="/dashboard/appearance/themes/import"`, `name="name"`, `new name`, `name="theme_json"`)
		}
		// Choosing a new name completes it, without picking the file again.
		_, body := e.importRaw(admin, map[string]string{"csrf": tok}, part{"theme", "t.json", strings.Replace(richTheme, `"rich"`, `"stripe"`, 1)})
		_, rest, _ := strings.Cut(body, `name="theme_json"`)
		_, val, _ := strings.Cut(rest, `value="`)
		hidden, _, _ := strings.Cut(val, `"`)
		r, _ := e.importRaw(admin, map[string]string{"csrf": tok, "name": "my-stripe", "theme_json": htmlUnescape(hidden)})
		if r.StatusCode != 303 {
			t.Fatalf("retry with a new name: %d", r.StatusCode)
		}
		if m := readManifest(t, e, "my-stripe"); m["version"] != "2.0.0" {
			t.Errorf("manifest: %v", m)
		}
		for _, n := range onDisk(t, e) {
			if n == "stripe" {
				t.Error("a directory shadowing stripe was written")
			}
		}
	})
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&#34;", `"`, "&quot;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&#43;", "+").Replace(s)
}

func TestImportRefusesBadFilesAndWritesNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		big := `{"name":"big","version":"1","description":"` + strings.Repeat("x", 70<<10) + `"}`
		huge := strings.Repeat("x", 200<<10)
		cases := []struct {
			what  string
			files []part
			code  int
			want  string
		}{
			{"not json", []part{{"theme", "t.json", "<html>"}}, 400, "theme.json"},
			{"empty", []part{{"theme", "t.json", ""}}, 400, "theme.json"},
			{"unknown field", []part{{"theme", "t.json", `{"name":"x1","version":"1","colour":{}}`}}, 400, "unknown field"},
			{"bad seed", []part{{"theme", "t.json", `{"name":"x2","version":"1","dashboard":{"seeds":{"accent":"red"}}}`}}, 400, "#rrggbb"},
			{"unsafe token", []part{{"theme", "t.json", `{"name":"x3","version":"1","tokens":{"--x":"url(http://evil.example/x)"}}`}}, 400, "not allowed"},
			{"bad name", []part{{"theme", "t.json", `{"name":"../evil","version":"1"}`}}, 400, "name"},
			{"over 64 KB", []part{{"theme", "t.json", big}}, 400, "64 KB"},
			{"no file", nil, 400, "theme.json"},
			{"wrong part name", []part{{"file", "t.json", `{"name":"x4","version":"1"}`}}, 400, "theme.json"},
			{"a zip is not a theme", []part{{"theme", "t.zip", "PK\x03\x04junk"}}, 400, "theme.json"},
		}
		for _, c := range cases {
			r, body := e.importRaw(admin, map[string]string{"csrf": tok}, c.files...)
			if r.StatusCode != c.code {
				t.Errorf("%s: %d, want %d", c.what, r.StatusCode, c.code)
				continue
			}
			has(t, body, c.want)
		}
		// Far past the cap, the body is cut off before it is parsed.
		if r, _ := e.importRaw(admin, map[string]string{"csrf": tok}, part{"theme", "t.json", huge}); r.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("huge: %d, want 413", r.StatusCode)
		}
		if got := onDisk(t, e); len(got) != 0 {
			t.Errorf("nothing should be written, found %v", got)
		}
	})
}

func TestImportTakesOnlyTheThemeJSON(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		r, _ := e.importRaw(admin, map[string]string{"csrf": tok},
			part{"theme", "theme.json", `{"name":"clean","version":"1.0.0","dashboard":{"seeds":{"accent":"#0d7d68"}}}`},
			part{"css", "evil.css", "body{display:none}"},
			part{"assets", "logo.svg", "<svg/>"},
			part{"templates", "pay.html", "<script>alert(1)</script>"})
		if r.StatusCode != 303 {
			t.Fatalf("import: %d", r.StatusCode)
		}
		entries, _ := os.ReadDir(filepath.Join(e.themesDir, "clean"))
		if len(entries) != 1 || entries[0].Name() != "theme.json" {
			t.Errorf("only theme.json may be written, found %v", entries)
		}
	})
}

func TestImportNeedsPermissionCSRFAndARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		file := part{"theme", "t.json", `{"name":"nope1","version":"1"}`}
		admin, viewer := e.roleClient("admin"), e.roleClient("viewer")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		if r, _ := e.importRaw(admin, map[string]string{}, file); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
		vtok := csrfOf(t, e.pageOK(viewer, "/dashboard/account"))
		if r, _ := e.importRaw(viewer, map[string]string{"csrf": vtok}, file); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		_ = tok
		if got := onDisk(t, e); len(got) != 0 {
			t.Errorf("written: %v", got)
		}
	})
}

func TestImportNeedsARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		c := staleAdmin(t, e.env)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))
		r, _ := e.importRaw(c, map[string]string{"csrf": tok}, part{"theme", "t.json", `{"name":"late","version":"1"}`})
		wantReauth(t, "POST import", r)
	})
}

func TestReservedNamesAreRefusedEverywhere(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("mine", tealAs("mine"), nil)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		for _, n := range []string{"import", "new"} {
			if r, _ := e.post(admin, "/dashboard/appearance/themes", themeForm(tok, "name", n)); r.StatusCode != 400 {
				t.Errorf("create %s: %d", n, r.StatusCode)
			}
			if r, _ := e.importRaw(admin, map[string]string{"csrf": tok, "name": n}, part{"theme", "t.json", `{"name":"ok1","version":"1"}`}); r.StatusCode != 400 {
				t.Errorf("import %s: %d", n, r.StatusCode)
			}
			if r, _ := e.post(admin, "/dashboard/appearance/themes/mine/rename", map[string][]string{"csrf": {tok}, "new_name": {n}}); r.StatusCode != 400 {
				t.Errorf("rename %s: %d", n, r.StatusCode)
			}
		}
	})
}

func TestImportNameFromTheFormRescuesAFileWithAnInvalidName(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/appearance"))
		bad := `{"name":"Not A Valid Name!","version":"1.0.0","dashboard":{"seeds":{"accent":"#0d7d68"}}}`
		// Without a name of its own, the file is refused for its name.
		if r, _ := e.importRaw(admin, map[string]string{"csrf": tok}, part{"theme", "t.json", bad}); r.StatusCode != 400 {
			t.Fatalf("invalid name, no override: %d", r.StatusCode)
		}
		// Typing a valid one is enough.
		r, _ := e.importRaw(admin, map[string]string{"csrf": tok, "name": "rescued"}, part{"theme", "t.json", bad})
		if r.StatusCode != 303 {
			t.Fatalf("with a valid name: %d", r.StatusCode)
		}
		if m := readManifest(t, e, "rescued"); m["name"] != "rescued" || m["version"] != "1.0.0" {
			t.Errorf("manifest: %v", m)
		}
		// An invalid override is still refused, and the file's content is kept in the form.
		r, body := e.importRaw(admin, map[string]string{"csrf": tok, "name": "Also Bad"}, part{"theme", "t.json", bad})
		if r.StatusCode != 400 {
			t.Errorf("invalid override: %d", r.StatusCode)
		}
		has(t, body, `name="theme_json"`)
	})
}
