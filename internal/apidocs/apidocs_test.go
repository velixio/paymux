package apidocs_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/paymux/paymux/internal/apidocs"
	"github.com/paymux/paymux/internal/web"
)

func realSpec(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func renderer(t *testing.T) *web.Renderer {
	t.Helper()
	r, err := web.NewThemedRenderer(web.ThemedOptions{Base: web.Embedded(), Themes: web.NewThemes(web.Bundled(), ""), Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newHandler(t *testing.T, spec []byte) *apidocs.Handler {
	t.Helper()
	h, err := apidocs.NewHandler(apidocs.Options{Spec: spec, Renderer: renderer(t)})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.HandlerFunc, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestEveryEndpointParameterAndSchemaOfTheRealSpecIsOnThePage(t *testing.T) {
	spec := realSpec(t)
	h := newHandler(t, spec)
	rec := get(h.Page, "GET", "/docs")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}

	var doc struct {
		Info       struct{ Title, Version string }
		Paths      map[string]map[string]yaml.Node
		Components struct {
			Schemas map[string]any
		}
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, doc.Info.Title) || !strings.Contains(body, doc.Info.Version) {
		t.Error("the page should carry the API's title and version")
	}
	ops := 0
	for path, methods := range doc.Paths {
		for method, node := range methods {
			var op struct {
				OperationID string `yaml:"operationId"`
				Summary     string
				Parameters  []struct {
					Name string
					Ref  string `yaml:"$ref"`
				}
				Responses map[string]any
			}
			if err := node.Decode(&op); err != nil {
				continue // path-level keys such as parameters
			}
			if op.OperationID == "" {
				continue
			}
			ops++
			for _, want := range []string{`id="op-` + op.OperationID + `"`, strings.ToUpper(method), path, op.Summary} {
				if !strings.Contains(body, want) {
					t.Errorf("%s %s: the page lacks %q", method, path, want)
				}
			}
			for _, p := range op.Parameters {
				if p.Name != "" && !strings.Contains(body, ">"+p.Name+"<") && !strings.Contains(body, "<code>"+p.Name+"</code>") {
					t.Errorf("%s %s: parameter %s is not documented", method, path, p.Name)
				}
			}
			for status := range op.Responses {
				if !strings.Contains(body, ">"+status+"<") {
					t.Errorf("%s %s: response %s is not documented", method, path, status)
				}
			}
		}
	}
	if ops < 20 {
		t.Errorf("only %d operations found on the page", ops)
	}
	var names []string
	for name := range doc.Components.Schemas {
		names = append(names, name)
		if !strings.Contains(body, `id="schema-`+name+`"`) {
			t.Errorf("schema %s has no section", name)
		}
	}
	sort.Strings(names)
	if len(names) < 10 {
		t.Errorf("schemas: %v", names)
	}
	// Payment links, the newest feature, are documented with examples.
	for _, want := range []string{"/v1/payment_links", "createPaymentLink", "single_use", "plink_"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestPageIsSelfContained(t *testing.T) {
	body := get(newHandler(t, realSpec(t)).Page, "GET", "/docs").Body.String()
	if strings.Contains(body, "<script") {
		t.Error("the reference needs no JavaScript")
	}
	ext := regexp.MustCompile(`(?i)(src|href|action)="https?://`)
	if m := ext.FindString(body); m != "" {
		t.Errorf("the page loads or links something external: %s", m)
	}
	if !strings.Contains(body, `href="/assets/css/app.css"`) || !strings.Contains(body, `href="/assets/css/docs.css"`) {
		t.Error("the page should use the shared layout and its own stylesheet")
	}
	if !strings.Contains(body, `href="/theme/default/theme.css`) {
		t.Error("the page should be themed")
	}
}

func TestTextIsEscapedAndSimpleMarkdownIsRendered(t *testing.T) {
	spec := `openapi: 3.1.0
info:
  title: "<b>Evil</b> API"
  version: "1"
  description: |
    First paragraph with **bold**, ` + "`code`" + ` and <script>alert(1)</script>.

    - one <i>item</i>
    - two
paths:
  /v1/things:
    get:
      operationId: listThings
      summary: List <things>
      description: 'A "quoted" & <raw> description'
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Thing" }
components:
  schemas:
    Thing:
      type: object
      properties:
        name: { type: string, description: "<img src=x onerror=alert(1)>" }
`
	body := get(newHandler(t, []byte(spec)).Page, "GET", "/docs").Body.String()
	for _, bad := range []string{"<script>alert", "<b>Evil", "<i>item", "<img src=x", "<things>", "<raw>"} {
		if strings.Contains(body, bad) {
			t.Errorf("unescaped markup %q reached the page", bad)
		}
	}
	for _, want := range []string{"&lt;b&gt;Evil&lt;/b&gt; API", "<strong>bold</strong>", "<code>code</code>", "&lt;script&gt;alert(1)&lt;/script&gt;",
		"<li>one &lt;i&gt;item&lt;/i&gt;</li>", "<li>two</li>", "List &lt;things&gt;", "&lt;img src=x onerror=alert(1)&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestExamplesAreValidJSONBuiltFromTheSchemas(t *testing.T) {
	doc, err := apidocs.Parse(realSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CreatePaymentRequest", "Payment", "PaymentLink", "CreatePaymentLinkRequest", "SubscriptionList", "CreatedSubscription", "Invoice"} {
		ex, err := doc.ExampleJSON(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(ex), &v); err != nil {
			t.Fatalf("%s: example is not JSON: %v\n%s", name, err, ex)
		}
		if len(v) == 0 {
			t.Errorf("%s: empty example", name)
		}
	}
	ex, _ := doc.ExampleJSON("PaymentLink")
	var link map[string]any
	_ = json.Unmarshal([]byte(ex), &link)
	if link["object"] != "payment_link" || link["currency"] == nil || !strings.HasPrefix(link["id"].(string), "plink_") && link["id"] == "" {
		t.Errorf("PaymentLink example %s", ex)
	}
	if _, err := doc.ExampleJSON("NoSuchSchema"); err == nil {
		t.Error("unknown schema should be an error")
	}
	// allOf is merged.
	ex, _ = doc.ExampleJSON("CreatedSubscription")
	if !strings.Contains(ex, "checkout_url") || !strings.Contains(ex, "billing_mode") {
		t.Errorf("allOf not merged: %s", ex)
	}
}

func TestRecursiveSchemasTerminate(t *testing.T) {
	spec := `openapi: 3.1.0
info: { title: T, version: "1" }
paths:
  /v1/tree:
    get:
      operationId: getTree
      summary: Tree
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Node" }
components:
  schemas:
    Node:
      type: object
      properties:
        name: { type: string }
        children:
          type: array
          items: { $ref: "#/components/schemas/Node" }
        parent: { $ref: "#/components/schemas/Node" }
`
	rec := get(newHandler(t, []byte(spec)).Page, "GET", "/docs")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="schema-Node"`) {
		t.Fatalf("%d", rec.Code)
	}
}

func TestBadSpecsAreRejectedAtStartup(t *testing.T) {
	for name, spec := range map[string]string{"not yaml": "a: [", "no paths": "openapi: 3.1.0\ninfo: {title: T, version: '1'}\n", "empty": ""} {
		if _, err := apidocs.NewHandler(apidocs.Options{Spec: []byte(spec), Renderer: renderer(t)}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRawSpecAndRouting(t *testing.T) {
	spec := realSpec(t)
	h := newHandler(t, spec)
	rec := get(h.Raw, "GET", "/docs/openapi.yaml")
	if rec.Code != 200 || rec.Body.String() != string(spec) || !strings.Contains(rec.Header().Get("Content-Type"), "yaml") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("raw: %d %v", rec.Code, rec.Header())
	}
	if rec := get(h.Raw, "HEAD", "/docs/openapi.yaml"); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD: %d %d", rec.Code, rec.Body.Len())
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		if rec := get(h.Page, m, "/docs"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /docs: %d", m, rec.Code)
		}
	}
	page := get(h.Page, "GET", "/docs")
	if page.Header().Get("X-Content-Type-Options") != "nosniff" || page.Header().Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(page.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("headers %v", page.Header())
	}
	// The page is built once and reused: two requests give the same bytes.
	if page.Body.String() != get(h.Page, "GET", "/docs").Body.String() {
		t.Error("the reference changed between requests")
	}
}
