package web

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func base() fstest.MapFS {
	return fstest.MapFS{
		"templates/layout.html": {Data: []byte(`{{define "layout"}}<html><body>{{template "content" .}}</body></html>{{end}}`)},
		"templates/hello.html":  {Data: []byte(`{{define "content"}}hello {{.Name}}{{end}}`)},
		"static/css/app.css":    {Data: []byte(`:root{--accent:#000}`)},
	}
}

func TestOverlayPrefersCustom(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "static/css"), 0o755)
	os.WriteFile(filepath.Join(dir, "static/css/app.css"), []byte(`:root{--accent:#f00}`), 0o644)
	o, err := NewOverlay(base(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(o, "static/css/app.css")
	if err != nil || string(b) != `:root{--accent:#f00}` {
		t.Fatalf("custom not preferred: %q %v", b, err)
	}
	if b, _ := fs.ReadFile(o, "templates/hello.html"); !bytes.Contains(b, []byte("hello")) {
		t.Fatal("embedded fallback missing")
	}
}

func TestOverlayMissingCustomDirIsFine(t *testing.T) {
	o, err := NewOverlay(base(), filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(o, "static/css/app.css"); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayBlocksEscape(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "custom")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(parent, "secret.txt"), filepath.Join(dir, "leak.txt"))
	o, _ := NewOverlay(base(), dir)
	if _, err := fs.ReadFile(o, "leak.txt"); err == nil {
		t.Fatal("symlink escape from custom dir was allowed")
	}
	if _, err := fs.ReadFile(o, "../secret.txt"); err == nil {
		t.Fatal("path traversal allowed")
	}
}

func TestRendererUsesOverlay(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	os.WriteFile(filepath.Join(dir, "templates/hello.html"), []byte(`{{define "content"}}custom {{.Name}}{{end}}`), 0o644)
	o, _ := NewOverlay(base(), dir)
	r, err := NewRenderer(o)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := r.Render(&buf, "hello", map[string]string{"Name": "<b>x</b>"}); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "custom &lt;b&gt;x&lt;/b&gt;") || !strings.HasPrefix(got, "<html>") {
		t.Fatalf("got %q", got)
	}
	if err := r.Render(&buf, "missing", nil); err == nil {
		t.Fatal("unknown page rendered")
	}
}

func TestRendererSharesUnderscorePartials(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
	os.WriteFile(filepath.Join(dir, "templates/_nav.html"), []byte(`{{define "nav"}}NAV[{{.Name}}]{{end}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "templates/p1.html"), []byte(`{{define "content"}}{{template "nav" .}} one{{end}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "templates/p2.html"), []byte(`{{define "content"}}{{template "nav" .}} two{{end}}`), 0o644)
	o, _ := NewOverlay(base(), dir)
	r, err := NewRenderer(o)
	if err != nil {
		t.Fatal(err)
	}
	for page, want := range map[string]string{"p1": "NAV[x] one", "p2": "NAV[x] two"} {
		var buf bytes.Buffer
		if err := r.Render(&buf, page, map[string]string{"Name": "x"}); err != nil || !strings.Contains(buf.String(), want) {
			t.Errorf("%s: %v %q", page, err, buf.String())
		}
	}
	if err := r.Render(&bytes.Buffer{}, "_nav", nil); err == nil {
		t.Fatal("a partial must not be a page")
	}
}

func TestEmbeddedPayTemplates(t *testing.T) {
	r, err := NewRenderer(Embedded())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err = r.Render(&buf, "pay_form_post", PayPage{
		AppName: "Voicestudio", Accent: "#123456",
		FormURL: "https://test.payu.in/_payment",
		Fields:  []Field{{Name: "key", Value: "k"}, {Name: "productinfo", Value: `"><script>alert(1)</script>`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "<script>alert(1)") {
		t.Fatal("field value not escaped")
	}
	if !strings.Contains(out, `action="https://test.payu.in/_payment"`) || !strings.Contains(out, `name="key"`) {
		t.Fatalf("form missing: %s", out)
	}
	buf.Reset()
	err = r.Render(&buf, "pay_sdk", PayPage{
		AppName: "Voicestudio", ScriptURL: "https://checkout.razorpay.com/v1/checkout.js", Launcher: "razorpay",
		Options: map[string]any{"key": "rzp_test_x", "order_id": "order_1", "name": "</script><script>alert(1)</script>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out = buf.String()
	if strings.Contains(out, "</script><script>alert(1)") {
		t.Fatal("sdk options not JS-escaped")
	}
	if !strings.Contains(out, `src="https://checkout.razorpay.com/v1/checkout.js"`) || !strings.Contains(out, "order_1") {
		t.Fatalf("sdk page wrong: %s", out)
	}
}

func TestAssetHandler(t *testing.T) {
	o, _ := NewOverlay(Embedded(), "")
	srv := httptest.NewServer(AssetHandler(o))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/assets/css/app.css")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("%v %v", res, err)
	}
	if res, _ := http.Get(srv.URL + "/assets/../templates/layout.html"); res.StatusCode == 200 {
		t.Fatal("templates reachable through assets")
	}
}
