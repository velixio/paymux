package web

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func i18nBase() fstest.MapFS {
	return fstest.MapFS{
		"templates/layout.html": {Data: []byte(`{{define "layout"}}<html lang="{{lang}}">{{template "content" .}}</html>{{end}}`)},
		"templates/hello.html":  {Data: []byte(`{{define "content"}}{{t "greet" .Name}}|{{t "only_en"}}|{{t "nowhere"}}|{{t "markup"}}|{{t "plain"}}{{end}}`)},
		"i18n/en.json":          {Data: []byte(`{"greet":"Hello %s","only_en":"English only","markup":"<b>bold</b>","plain":"plain"}`)},
		"i18n/hi.json":          {Data: []byte(`{"greet":"नमस्ते %s","plain":"सादा"}`)},
	}
}

func renderHello(t *testing.T, r *Renderer) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.Render(&b, "hello", map[string]string{"Name": "Asha"}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTemplateHelperTranslatesFallsBackToEnglishAndToTheKey(t *testing.T) {
	r, err := NewRenderer(i18nBase())
	if err != nil {
		t.Fatal(err)
	}
	if got := renderHello(t, r); !strings.Contains(got, `lang="en"`) || !strings.Contains(got, "Hello Asha|English only|nowhere|") {
		t.Fatalf("default language is English: %s", got)
	}
	got := renderHello(t, r.With("", "hi"))
	if !strings.Contains(got, `lang="hi"`) || !strings.Contains(got, "नमस्ते Asha|English only|nowhere|") || !strings.Contains(got, "|सादा") {
		t.Fatalf("hi: %s", got)
	}
	if strings.Contains(got, "<b>") || !strings.Contains(got, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Fatalf("translations must be HTML-escaped: %s", got)
	}
	// An unknown language is English, and says so.
	if got := renderHello(t, r.With("", "xx")); !strings.Contains(got, "Hello Asha") || !strings.Contains(got, `lang="en"`) {
		t.Fatalf("unknown language: %s", got)
	}
}

func TestRendererTAndHasLang(t *testing.T) {
	r, _ := NewRenderer(i18nBase())
	if got := r.T("hi", "greet", "Ravi"); got != "नमस्ते Ravi" {
		t.Fatal(got)
	}
	if got := r.T("hi", "only_en"); got != "English only" {
		t.Fatal(got)
	}
	if got := r.T("zz", "plain"); got != "plain" {
		t.Fatal(got)
	}
	if got := r.T("en", "missing.key"); got != "missing.key" {
		t.Fatal(got)
	}
	for lang, want := range map[string]bool{"en": true, "hi": true, "xx": false, "": false, "../en": false, "EN": false, "en.json": false} {
		if r.HasLang(lang) != want {
			t.Errorf("HasLang(%q) = %v, want %v", lang, !want, want)
		}
	}
	if got := r.Languages(); strings.Join(got, ",") != "en,hi" {
		t.Fatalf("Languages: %v", got)
	}
}

func TestCustomDirCanAddAndOverrideLanguages(t *testing.T) {
	custom := t.TempDir()
	if err := os.MkdirAll(filepath.Join(custom, "i18n"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(custom, "i18n", "fr.json"), []byte(`{"greet":"Bonjour %s"}`), 0o644)
	os.WriteFile(filepath.Join(custom, "i18n", "en.json"), []byte(`{"plain":"custom plain"}`), 0o644)
	r, err := NewThemedRenderer(ThemedOptions{Base: i18nBase(), CustomDir: custom, Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasLang("fr") || r.T("fr", "greet", "Zoe") != "Bonjour Zoe" {
		t.Fatal("custom language not available")
	}
	if got := r.T("en", "plain"); got != "custom plain" {
		t.Fatalf("custom en override: %q", got)
	}
	if got := r.T("en", "greet", "Zoe"); got != "Hello Zoe" {
		t.Fatalf("custom en.json should merge over the embedded one, not replace it: %q", got)
	}
}

func TestBrokenCatalogIsIgnoredAndAlerted(t *testing.T) {
	base := i18nBase()
	base["i18n/hi.json"] = &fstest.MapFile{Data: []byte(`{"greet":`)}
	al := &alertLog{}
	r, err := NewThemedRenderer(ThemedOptions{Base: base, Alert: al.add, Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.T("hi", "greet", "Asha"); got != "Hello Asha" {
		t.Fatalf("a broken catalog falls back to English: %q", got)
	}
	r.T("hi", "greet", "Asha")
	if al.count() != 1 {
		t.Fatalf("want one alert, got %v", al.n)
	}
}

// ---- the shipped catalogs ----

var verbRE = regexp.MustCompile(`%(\[[0-9]+\])?[sdv]`)

func loadCatalog(t *testing.T, lang string) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(Embedded(), "i18n/"+lang+".json")
	if err != nil {
		t.Fatalf("catalog %s: %v", lang, err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("catalog %s: %v", lang, err)
	}
	return m
}

func TestShippedCatalogsAreComplete(t *testing.T) {
	en, hi := loadCatalog(t, "en"), loadCatalog(t, "hi")
	var problems []string
	for k, v := range en {
		h, ok := hi[k]
		switch {
		case !ok || strings.TrimSpace(h) == "":
			problems = append(problems, "hi lacks "+k)
		case len(verbRE.FindAllString(v, -1)) != len(verbRE.FindAllString(h, -1)):
			problems = append(problems, "format verbs differ in "+k)
		}
	}
	for k := range hi {
		if _, ok := en[k]; !ok {
			problems = append(problems, "hi has a key en lacks: "+k)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatal(strings.Join(problems, "\n"))
	}
	if len(en) < 30 {
		t.Fatalf("suspiciously few messages: %d", len(en))
	}
}

var tKeyRE = regexp.MustCompile(`\{\{\s*t\s+"([^"]+)"`)

func TestEveryKeyUsedByAShippedTemplateExists(t *testing.T) {
	en := loadCatalog(t, "en")
	used := 0
	err := fs.WalkDir(Embedded(), "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := fs.ReadFile(Embedded(), p)
		for _, m := range tKeyRE.FindAllStringSubmatch(string(raw), -1) {
			used++
			if _, ok := en[m[1]]; !ok {
				t.Errorf("%s uses {{t %q}} which en.json lacks", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if used < 10 {
		t.Fatalf("the customer templates should be translated; found %d {{t}} uses", used)
	}
}

func TestCustomerPagesRenderInHindi(t *testing.T) {
	r, err := NewRenderer(Embedded())
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := r.With("", "hi").Render(&b, "pay_billing", PayPage{AppName: "Shop"}); err != nil {
		t.Fatal(err)
	}
	hi := loadCatalog(t, "hi")
	for _, key := range []string{"billing.title", "billing.name", "billing.continue"} {
		if hi[key] == "" || !strings.Contains(b.String(), hi[key]) {
			t.Errorf("pay_billing in hi lacks %q (%q)", key, hi[key])
		}
	}
	if !strings.Contains(b.String(), `lang="hi"`) {
		t.Error("the page should declare its language")
	}
}
