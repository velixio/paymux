package web

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func writeTheme(t *testing.T, dir, name string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, name, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func manifest(name string) string {
	return `{"name":"` + name + `","version":"1.0.0","tokens":{"--color-accent":"#ff0066","--radius":"2px"},"font_stack":"Georgia, serif"}`
}

func TestBundledThemesLoad(t *testing.T) {
	th := NewThemes(Bundled(), "")
	for _, name := range []string{"default", "minimal-dark"} {
		got, err := th.Load(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Name != name || got.Version == "" || !got.Bundled {
			t.Fatalf("%s: %+v", name, got.Manifest)
		}
	}
	d, _ := th.Load("minimal-dark")
	if d.Tokens["--color-bg"] == "" {
		t.Fatal("minimal-dark should override the background token")
	}
	var names []string
	for _, i := range th.List() {
		names = append(names, i.Name)
	}
	if strings.Join(names, ",") != "default,minimal-dark" {
		t.Fatalf("list: %v", names)
	}
}

func TestParseManifestValidation(t *testing.T) {
	cases := []struct{ name, json, dir, want string }{
		{"ok", manifest("mine"), "mine", ""},
		{"not json", `{`, "mine", "theme.json"},
		{"unknown field", `{"name":"mine","version":"1","colour":{}}`, "mine", "unknown field"},
		{"name differs from directory", manifest("other"), "mine", "directory"},
		{"bad name", manifest("My Theme"), "My Theme", "name"},
		{"no version", `{"name":"mine"}`, "mine", "version"},
		{"token not a custom property", `{"name":"mine","version":"1","tokens":{"color":"red"}}`, "mine", "token"},
		{"token value with semicolon", `{"name":"mine","version":"1","tokens":{"--x":"red; } body { display:none"}}`, "mine", "value"},
		{"token value with url()", `{"name":"mine","version":"1","tokens":{"--x":"url(http://evil.example.com/x)"}}`, "mine", "value"},
		{"token value with comment", `{"name":"mine","version":"1","tokens":{"--x":"red/*"}}`, "mine", "value"},
		{"token value with angle bracket", `{"name":"mine","version":"1","tokens":{"--x":"</style>"}}`, "mine", "value"},
		{"font stack with brace", `{"name":"mine","version":"1","font_stack":"a}b"}`, "mine", "font_stack"},
	}
	for _, c := range cases {
		_, err := ParseManifest([]byte(c.json), c.dir)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestDataDirThemeShadowsBundledAndInvalidOnesAreListedWithTheirError(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "default", map[string]string{"theme.json": manifest("default")})
	writeTheme(t, dir, "broken", map[string]string{"theme.json": `{"name":"broken"}`})
	th := NewThemes(Bundled(), dir)
	d, err := th.Load("default")
	if err != nil || d.Bundled || d.Tokens["--color-accent"] != "#ff0066" {
		t.Fatalf("data dir theme should win: %+v %v", d, err)
	}
	if _, err := th.Load("broken"); err == nil {
		t.Fatal("broken theme loaded")
	}
	var broken *ThemeInfo
	for _, i := range th.List() {
		if i.Name == "broken" {
			broken = &i
		}
	}
	if broken == nil || broken.Err == "" {
		t.Fatalf("broken theme should be listed with its error: %+v", th.List())
	}
	if _, err := th.Load("../etc"); err == nil {
		t.Fatal("path traversal accepted as a theme name")
	}
	if _, err := th.Load("nope"); err == nil {
		t.Fatal("unknown theme loaded")
	}
}

func themedBase() fstest.MapFS {
	return fstest.MapFS{
		"templates/layout.html": {Data: []byte(`{{define "layout"}}<html><head><link rel="stylesheet" href="{{themeCSS}}"></head><body>{{template "content" .}}</body></html>{{end}}`)},
		"templates/hello.html":  {Data: []byte(`{{define "content"}}hello {{.Name}}{{end}}`)},
		"templates/other.html":  {Data: []byte(`{{define "content"}}embedded other{{end}}`)},
	}
}

type alertLog struct {
	mu sync.Mutex
	n  []string
}

func (a *alertLog) add(theme string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n = append(a.n, theme+": "+err.Error())
}
func (a *alertLog) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.n) }

func render(t *testing.T, r *Renderer) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.Render(&b, "hello", map[string]string{"Name": "x"}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestThemedRendererOverlayOrderIsEmbeddedThemeCustom(t *testing.T) {
	data := t.TempDir()
	themes := filepath.Join(data, "themes")
	custom := filepath.Join(data, "custom")
	writeTheme(t, themes, "mine", map[string]string{
		"theme.json":           manifest("mine"),
		"templates/hello.html": `{{define "content"}}theme says hello {{.Name}}{{end}}`,
		"templates/other.html": `{{define "content"}}theme other{{end}}`,
	})
	writeTheme(t, custom, "", map[string]string{"templates/other.html": `{{define "content"}}custom other{{end}}`})
	al := &alertLog{}
	r, err := NewThemedRenderer(ThemedOptions{Base: themedBase(), CustomDir: custom, Themes: NewThemes(Bundled(), themes), Alert: al.add, Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	page := func(r *Renderer, name string) string {
		var b bytes.Buffer
		if err := r.Render(&b, name, map[string]string{"Name": "x"}); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if got := page(r, "hello"); !strings.Contains(got, "hello x") || strings.Contains(got, "theme says") {
		t.Fatalf("no theme should be the embedded page: %s", got)
	}
	if got := page(r.With("mine", ""), "hello"); !strings.Contains(got, "theme says hello x") {
		t.Fatalf("active theme should override embedded: %s", got)
	}
	if !strings.Contains(page(r.With("mine", ""), "hello"), `/theme/mine/theme.css`) {
		t.Fatal("layout should link the theme stylesheet")
	}
	if got := page(r.With("mine", ""), "other"); !strings.Contains(got, "custom other") {
		t.Fatalf("custom should beat the theme: %s", got)
	}
	if got := page(r, "other"); !strings.Contains(got, "custom other") {
		t.Fatalf("custom should beat embedded: %s", got)
	}
	if al.count() != 0 {
		t.Fatalf("unexpected alerts: %v", al.n)
	}
}

func TestBadThemeFallsBackToEmbeddedAndAlertsOnce(t *testing.T) {
	data := t.TempDir()
	themes := filepath.Join(data, "themes")
	writeTheme(t, themes, "badjson", map[string]string{"theme.json": `{"name":"badjson"}`})
	writeTheme(t, themes, "badtpl", map[string]string{"theme.json": manifest("badtpl"), "templates/hello.html": `{{define "content"}}{{.Name`})
	al := &alertLog{}
	r, err := NewThemedRenderer(ThemedOptions{Base: themedBase(), Themes: NewThemes(Bundled(), themes), Alert: al.add, Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"badjson", "badtpl", "missing"} {
		for i := 0; i < 3; i++ {
			if got := render(t, r.With(name, "")); !strings.Contains(got, "hello x") {
				t.Fatalf("%s should fall back to the embedded pages: %s", name, got)
			}
		}
	}
	if al.count() != 3 {
		t.Fatalf("want one alert per bad theme, got %d: %v", al.count(), al.n)
	}
	// Fixing the theme on disk recovers without a restart.
	writeTheme(t, themes, "badtpl", map[string]string{"templates/hello.html": `{{define "content"}}fixed {{.Name}}{{end}}`})
	if got := render(t, r.With("badtpl", "")); !strings.Contains(got, "fixed x") {
		t.Fatalf("fixed theme not picked up: %s", got)
	}
}

func TestThemeChangesApplyWithoutRestart(t *testing.T) {
	data := t.TempDir()
	themes := filepath.Join(data, "themes")
	writeTheme(t, themes, "mine", map[string]string{"theme.json": manifest("mine"), "templates/hello.html": `{{define "content"}}v1{{end}}`})
	r, err := NewThemedRenderer(ThemedOptions{Base: themedBase(), Themes: NewThemes(Bundled(), themes), Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, r.With("mine", "")); !strings.Contains(got, "v1") {
		t.Fatal(got)
	}
	writeTheme(t, themes, "mine", map[string]string{"templates/hello.html": `{{define "content"}}version two{{end}}`})
	if got := render(t, r.With("mine", "")); !strings.Contains(got, "version two") {
		t.Fatalf("edit not picked up: %s", got)
	}
}

func TestThemeHandlerServesTokensCSSAndAssets(t *testing.T) {
	data := t.TempDir()
	themes := filepath.Join(data, "themes")
	writeTheme(t, themes, "mine", map[string]string{
		"theme.json":    manifest("mine"),
		"css/a.css":     `.a{color:red}`,
		"css/b.css":     `.b{color:blue}`,
		"assets/l.svg":  `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"secret.txt":    `nope`,
		"css/notes.txt": `ignored`,
	})
	h := NewThemes(Bundled(), themes).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	w := get("/theme/mine/theme.css")
	body := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("theme.css: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for _, want := range []string{"--color-accent:#ff0066;", "--radius:2px;", "--font:Georgia, serif;", ".a{color:red}", ".b{color:blue}"} {
		if !strings.Contains(body, want) {
			t.Errorf("theme.css lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "ignored") {
		t.Error("non-css files must not be concatenated")
	}
	if strings.Index(body, ".a{") > strings.Index(body, ".b{") {
		t.Error("css files should be in name order")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("ETag") == "" {
		t.Errorf("headers: %v", w.Header())
	}
	if w := get("/theme/mine/assets/l.svg"); w.Code != 200 {
		t.Errorf("asset: %d", w.Code)
	}
	for _, p := range []string{"/theme/mine/secret.txt", "/theme/mine/theme.json", "/theme/mine/assets/../secret.txt", "/theme/mine/assets/nope.svg",
		"/theme/nope/theme.css", "/theme/..%2fmine/theme.css", "/theme/Bad%20Name/theme.css"} {
		if w := get(p); w.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", p, w.Code)
		}
	}
	if w := get("/theme/default/theme.css"); w.Code != 200 {
		t.Errorf("bundled default theme.css: %d", w.Code)
	}
}

func TestThemeCSSReflectsEditsAndETagChanges(t *testing.T) {
	themes := filepath.Join(t.TempDir(), "themes")
	writeTheme(t, themes, "mine", map[string]string{"theme.json": manifest("mine"), "css/a.css": `.a{}`})
	th := NewThemes(Bundled(), themes)
	h := th.Handler()
	etag := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/theme/mine/theme.css", nil))
		return w.Header().Get("ETag")
	}
	before := etag()
	writeTheme(t, themes, "mine", map[string]string{"css/a.css": `.a{color:green}`})
	if after := etag(); after == before {
		t.Fatal("etag should change when a css file changes")
	}
}

func TestEmbeddedLayoutLinksTheTheme(t *testing.T) {
	r, err := NewThemedRenderer(ThemedOptions{Base: Embedded(), Themes: NewThemes(Bundled(), ""), Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := r.With("minimal-dark", "").Render(&b, "pay_result", PayPage{Title: "T", Message: "M"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `href="/theme/minimal-dark/theme.css`) {
		t.Fatalf("pay_result should link the theme CSS: %s", b.String())
	}
}

var _ fs.FS = (*Overlay)(nil)

func dashManifest(block string) string {
	return `{"name":"mine","version":"1.0.0","dashboard":` + block + `}`
}

func TestParseManifestDashboardBlock(t *testing.T) {
	cases := []struct{ name, block, want string }{
		{"seeds only", `{"seeds":{"accent":"#635bff"}}`, ""},
		{"all seeds, dark seeds, scheme, overrides", `{"seeds":{"accent":"#635bff","background":"#ffffff","sidebar":"#f7f8fa","text":"#1a1f36","sidebar_text":"#3c4257"},"dark_seeds":{"accent":"#8b85ff","background":"#0f1117"},"scheme":"dark","overrides":{"light":{"--border":"#e3e8ee"},"dark":{"--ring":"#8b85ff"}}}`, ""},
		{"empty scheme is allowed", `{"seeds":{"accent":"#635bff"},"scheme":""}`, ""},
		{"accent is required", `{"seeds":{"background":"#ffffff"}}`, "accent"},
		{"dark seeds need an accent too", `{"seeds":{"accent":"#635bff"},"dark_seeds":{"background":"#000000"}}`, "accent"},
		{"bad hex", `{"seeds":{"accent":"red"}}`, "#rrggbb"},
		{"short hex", `{"seeds":{"accent":"#fff"}}`, "#rrggbb"},
		{"bad hex in a later seed", `{"seeds":{"accent":"#635bff","text":"#12345g"}}`, "text"},
		{"bad hex in dark seeds", `{"seeds":{"accent":"#635bff"},"dark_seeds":{"accent":"#xyz"}}`, "dark_seeds"},
		{"unknown scheme", `{"seeds":{"accent":"#635bff"},"scheme":"sepia"}`, "scheme"},
		{"status token in overrides", `{"seeds":{"accent":"#635bff"},"overrides":{"light":{"--status-ok-bg":"#00ff00"}}}`, "--status-ok-bg"},
		{"unknown token in overrides", `{"seeds":{"accent":"#635bff"},"overrides":{"light":{"--nope":"#00ff00"}}}`, "--nope"},
		{"override value is not hex", `{"seeds":{"accent":"#635bff"},"overrides":{"dark":{"--border":"url(x)"}}}`, "#rrggbb"},
		{"unknown override mode", `{"seeds":{"accent":"#635bff"},"overrides":{"sepia":{"--border":"#000000"}}}`, "overrides"},
		{"unknown field in block", `{"seeds":{"accent":"#635bff"},"extra":1}`, "unknown field"},
	}
	for _, c := range cases {
		_, err := ParseManifest([]byte(dashManifest(c.block)), "mine")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestDashboardTokensExcludeStatusColours(t *testing.T) {
	if len(DashboardTokens) == 0 {
		t.Fatal("DashboardTokens is empty")
	}
	if !slices.Contains(DashboardTokens, "--sidebar-foreground") {
		t.Error("--sidebar-foreground must be themeable")
	}
	for _, k := range DashboardTokens {
		if strings.HasPrefix(k, "--status-") {
			t.Errorf("%s must not be themeable", k)
		}
	}
}

func TestDashboardJSONEndpoint(t *testing.T) {
	themes := t.TempDir()
	writeTheme(t, themes, "mine", map[string]string{"theme.json": dashManifest(`{"seeds":{"accent":"#635bff","background":"#ffffff"},"dark_seeds":{"accent":"#8b85ff"},"scheme":"dark","overrides":{"light":{"--border":"#e3e8ee"}}}`)})
	writeTheme(t, themes, "plain", map[string]string{"theme.json": `{"name":"plain","version":"1.0.0"}`})
	h := NewThemes(Bundled(), themes).Handler()
	get := func(p string, hdr ...string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		if len(hdr) == 1 {
			r.Header.Set("If-None-Match", hdr[0])
		}
		h.ServeHTTP(w, r)
		return w
	}

	w := get("/theme/mine/dashboard.json")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache control %q", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Error("no ETag")
	}
	var got struct {
		Name      string                       `json:"name"`
		Scheme    string                       `json:"scheme"`
		Seeds     map[string]string            `json:"seeds"`
		DarkSeeds map[string]string            `json:"dark_seeds"`
		Overrides map[string]map[string]string `json:"overrides"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	if got.Name != "mine" || got.Scheme != "dark" || got.Seeds["accent"] != "#635bff" || got.Seeds["background"] != "#ffffff" ||
		got.DarkSeeds["accent"] != "#8b85ff" || got.Overrides["light"]["--border"] != "#e3e8ee" {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
	if _, has := got.Seeds["sidebar"]; has {
		t.Errorf("unset seeds should be omitted: %s", w.Body.String())
	}
	if w2 := get("/theme/mine/dashboard.json", etag); w2.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d", w2.Code)
	}

	// A theme without a dashboard block still answers, with empty seeds.
	w = get("/theme/plain/dashboard.json")
	if w.Code != 200 {
		t.Fatalf("plain: status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"seeds":{}`) || !strings.Contains(w.Body.String(), `"overrides":{}`) {
		t.Errorf("plain: want empty seeds and overrides, got %s", w.Body.String())
	}

	if w = get("/theme/nope/dashboard.json"); w.Code != 404 {
		t.Errorf("missing theme: status %d", w.Code)
	}
	if w = httptest.NewRecorder(); true {
		r := httptest.NewRequest("GET", "/theme/mine/dashboard.json", nil)
		h.ServeHTTP(w, r)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("nosniff header missing")
		}
	}
}
