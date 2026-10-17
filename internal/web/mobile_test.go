package web

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The mobile pass: every template and stylesheet is checked for a 360px wide
// screen. There is no browser in the test run, so these are structural checks
// (what the CSS says at that width, what the templates contain) that catch the
// usual causes of horizontal scrolling and cramped forms. A person still looks
// at the pages on a phone.

const phone = 360

// ---- a small CSS reader ----

type cssRule struct {
	media []string
	sel   string
	decls [][2]string // in source order
}

var commentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

// parseCSS reads rules and @media blocks; other at-rules are skipped.
func parseCSS(src string) []cssRule {
	src = commentRE.ReplaceAllString(src, "")
	var out []cssRule
	var walk func(s string, media []string)
	walk = func(s string, media []string) {
		for {
			i := strings.IndexByte(s, '{')
			if i < 0 {
				return
			}
			prelude := strings.TrimSpace(s[:i])
			depth, j := 1, i+1
			for j < len(s) && depth > 0 {
				switch s[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
				j++
			}
			body := s[i+1 : j-1]
			s = s[j:]
			switch {
			case strings.HasPrefix(prelude, "@media"):
				walk(body, append(append([]string(nil), media...), strings.TrimSpace(strings.TrimPrefix(prelude, "@media"))))
			case strings.HasPrefix(prelude, "@"):
				// @font-face, @keyframes: nothing about layout widths
			default:
				r := cssRule{media: media, sel: prelude}
				for _, d := range strings.Split(body, ";") {
					if k, v, ok := strings.Cut(d, ":"); ok && strings.TrimSpace(k) != "" {
						r.decls = append(r.decls, [2]string{strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)})
					}
				}
				out = append(out, r)
			}
		}
	}
	walk(src, nil)
	return out
}

var widthCondRE = regexp.MustCompile(`\((max|min)-width:\s*(\d+)px\)`)

// appliesAt reports whether the rule's media conditions hold at a viewport
// width in px; conditions about colour scheme or motion are taken as true.
func (r cssRule) appliesAt(w int) bool {
	for _, cond := range r.media {
		for _, m := range widthCondRE.FindAllStringSubmatch(cond, -1) {
			n, _ := strconv.Atoi(m[2])
			if m[1] == "max" && w > n || m[1] == "min" && w < n {
				return false
			}
		}
	}
	return true
}

// effective is what a selector ends up with at a width: later declarations of
// the same selector and property win (specificity is not modelled).
func effective(rules []cssRule, w int) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, r := range rules {
		if !r.appliesAt(w) {
			continue
		}
		for _, sel := range strings.Split(r.sel, ",") {
			sel = strings.TrimSpace(sel)
			if out[sel] == nil {
				out[sel] = map[string]string{}
			}
			for _, d := range r.decls {
				out[sel][d[0]] = d[1]
			}
		}
	}
	return out
}

var lengthRE = regexp.MustCompile(`(-?\d+(?:\.\d+)?)(px|rem|em)\b`)

// pxIn returns the lengths in a value as px (em and rem count 16px).
func pxIn(v string) []float64 {
	var out []float64
	for _, m := range lengthRE.FindAllStringSubmatch(v, -1) {
		n, _ := strconv.ParseFloat(m[1], 64)
		if m[2] != "px" {
			n *= 16
		}
		out = append(out, n)
	}
	return out
}

func loadCSS(t *testing.T) (all []cssRule, perFile map[string][]cssRule) {
	t.Helper()
	perFile = map[string][]cssRule{}
	files, err := fs.Glob(Embedded(), "static/css/*.css")
	if err != nil || len(files) < 3 {
		t.Fatalf("stylesheets: %v %v", files, err)
	}
	for _, f := range files {
		raw, _ := fs.ReadFile(Embedded(), f)
		rules := parseCSS(string(raw))
		perFile[path.Base(f)] = rules
		all = append(all, rules...)
	}
	return all, perFile
}

func templates(t *testing.T) map[string]string {
	t.Helper()
	files, _ := fs.Glob(Embedded(), "templates/*.html")
	if len(files) < 30 {
		t.Fatalf("only %d templates found", len(files))
	}
	out := map[string]string{}
	for _, f := range files {
		raw, _ := fs.ReadFile(Embedded(), f)
		out[path.Base(f)] = string(raw)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- checks ----

func TestNoFixedWidthWiderThanAPhone(t *testing.T) {
	all, _ := loadCSS(t)
	eff := effective(all, phone)
	for _, sel := range sortedKeys(eff) {
		for _, prop := range []string{"width", "min-width", "flex-basis"} {
			if v, ok := eff[sel][prop]; ok {
				for _, px := range pxIn(v) {
					if px > phone {
						t.Errorf("%s { %s: %s } is %vpx at a %dpx screen", sel, prop, v, px, phone)
					}
				}
			}
		}
		if v, ok := eff[sel]["grid-template-columns"]; ok {
			sum := 0.0
			for _, px := range pxIn(v) {
				sum += px
			}
			if sum > 200 {
				t.Errorf("%s { grid-template-columns: %s } fixes %vpx of columns at %dpx", sel, v, sum, phone)
			}
		}
	}
	// Whatever is wider than a phone must be asked to fit (images, controls, code).
	for _, need := range []struct{ sel, prop string }{{"img", "max-width"}, {"svg", "max-width"}, {"input", "max-width"}, {"select", "max-width"}, {"textarea", "max-width"}} {
		found := false
		for sel, decls := range eff {
			for _, s := range strings.Split(sel, ",") {
				if strings.TrimSpace(s) == need.sel && strings.HasPrefix(decls[need.prop], "100%") {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("no rule gives %s %s: 100%%", need.sel, need.prop)
		}
	}
}

func TestPageChromeStacksOnAPhone(t *testing.T) {
	all, _ := loadCSS(t)
	eff := effective(all, phone)
	body := eff[".dash-body"]
	if body["flex-direction"] != "column" && body["display"] != "block" {
		t.Errorf(".dash-body at %dpx: %v: the navigation must stack above the page", phone, body)
	}
	if w := eff[".dash-nav"]["width"]; w != "auto" && w != "100%" {
		t.Errorf(".dash-nav width at %dpx is %q: a 180px side bar leaves half a phone for the page", phone, w)
	}
	if cols := eff[".docs-layout"]["grid-template-columns"]; len(strings.Fields(strings.ReplaceAll(cols, "minmax(0, 1fr)", "1fr"))) != 1 {
		t.Errorf(".docs-layout columns at %dpx: %q: the API reference needs one column", phone, cols)
	}
	if cols := eff["dl.facts"]["grid-template-columns"]; len(strings.Fields(strings.ReplaceAll(cols, "minmax(0, 1fr)", "1fr"))) > 1 {
		t.Errorf("dl.facts columns at %dpx: %q: label and value should stack", phone, cols)
	}
	if eff["main.dash"]["padding"] == "" && eff[".dash-main"]["padding"] == "" {
		t.Error("the dashboard page has no padding rule")
	}
	if p := eff[".dash-main"]["padding"]; p != "" {
		for _, px := range pxIn(p) {
			if px > 20 {
				t.Errorf(".dash-main padding %q at %dpx wastes the width", p, phone)
			}
		}
	}
}

func TestEveryTableScrollsInsideItsOwnContainer(t *testing.T) {
	tags := regexp.MustCompile(`(?s)<div\b[^>]*>|</div>|<table\b[^>]*>|</table>`)
	tables := 0
	for _, name := range sortedKeys(templates(t)) {
		src := templates(t)[name]
		var stack []bool
		for _, m := range tags.FindAllString(src, -1) {
			switch {
			case strings.HasPrefix(m, "</div"):
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			case strings.HasPrefix(m, "<div"):
				stack = append(stack, strings.Contains(m, "table-scroll"))
			case strings.HasPrefix(m, "<table"):
				tables++
				inside := false
				for _, s := range stack {
					inside = inside || s
				}
				if !inside {
					t.Errorf("%s: %s is not inside <div class=\"table-scroll\">", name, m)
				}
			}
		}
	}
	if tables < 25 {
		t.Errorf("only %d tables found; the scan is probably broken", tables)
	}
	all, perFile := loadCSS(t)
	eff := effective(perFile["app.css"], phone)
	if v := eff[".table-scroll"]["overflow-x"]; v != "auto" && v != "scroll" {
		t.Errorf("app.css (loaded by every page, customer pages included) must define .table-scroll { overflow-x: auto }; got %q", v)
	}
	if v := effective(all, phone)[".table-scroll"]["max-width"]; v != "100%" {
		t.Errorf(".table-scroll max-width is %q, want 100%%", v)
	}
}

var controlRE = regexp.MustCompile(`<(input|select|textarea)\b([^>]*)>`)

func attr(attrs, name string) string {
	m := regexp.MustCompile(`\b` + name + `="([^"]*)"`).FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestFormControlsFillTheWidth(t *testing.T) {
	all, _ := loadCSS(t)
	eff := effective(all, phone)

	// .field is the control of the customer pages and most forms.
	if f := eff[".field"]; f["width"] != "100%" || f["display"] != "block" {
		t.Errorf(".field at %dpx: %v, want a full-width block", phone, f)
	}
	if b := eff["*"]["box-sizing"]; b != "border-box" {
		t.Errorf("box-sizing is %q: padding would push 100%% controls past the edge", b)
	}
	// Dashboard forms without .field (filters, inline editors) fill the width through one rule.
	covered := func(tag string) bool {
		for sel, d := range eff {
			if strings.Contains(sel, ".dash-main") && strings.Contains(sel, tag) && d["width"] == "100%" {
				return true
			}
		}
		return false
	}
	for _, tag := range []string{"select", "textarea", "input"} {
		if !covered(tag) {
			t.Errorf("no rule makes dashboard %s controls full width at %dpx", tag, phone)
		}
	}
	// A .filters form stacks its controls on a phone.
	if d := eff["form.filters"]; d["flex-direction"] != "column" && d["display"] != "block" {
		t.Errorf("form.filters at %dpx: %v: controls should stack", phone, d)
	}

	customer := 0
	for _, name := range sortedKeys(templates(t)) {
		if strings.HasPrefix(name, "dash_") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, "email") {
			continue
		}
		customer++
		for _, m := range controlRE.FindAllStringSubmatch(templates(t)[name], -1) {
			switch attr(m[2], "type") {
			case "hidden", "checkbox", "radio", "submit", "button":
				continue
			}
			if !strings.Contains(" "+attr(m[2], "class")+" ", " field ") {
				t.Errorf("%s: <%s %s> needs class=\"field\" (customer pages have no other form styling)", name, m[1], strings.TrimSpace(m[2]))
			}
		}
	}
	if customer < 5 {
		t.Errorf("only %d customer templates scanned", customer)
	}
	for _, name := range sortedKeys(templates(t)) {
		src := templates(t)[name]
		for _, m := range controlRE.FindAllStringSubmatch(src, -1) {
			if sz := attr(m[2], "size"); sz != "" {
				n, _ := strconv.Atoi(sz)
				if n > 30 && !strings.Contains(" "+attr(m[2], "class")+" ", " field ") && !strings.Contains(src, "filters") {
					t.Errorf("%s: <%s size=%s> is wider than a phone unless the control is told to fit", name, m[1], sz)
				}
			}
		}
	}
}

func TestNothingElseForcesAWidth(t *testing.T) {
	widthAttr := regexp.MustCompile(`\b(width|height)="(\d+)"`)
	inlineStyle := regexp.MustCompile(`style="[^"]*(?:min-)?width:\s*(\d+)px`)
	for _, name := range sortedKeys(templates(t)) {
		src := templates(t)[name]
		for _, m := range inlineStyle.FindAllStringSubmatch(src, -1) {
			if n, _ := strconv.Atoi(m[1]); n > phone {
				t.Errorf("%s: inline style width %dpx", name, n)
			}
		}
		for _, tag := range regexp.MustCompile(`<(img|svg|iframe|video|canvas)\b[^>]*>`).FindAllString(src, -1) {
			for _, m := range widthAttr.FindAllStringSubmatch(tag, -1) {
				if n, _ := strconv.Atoi(m[2]); m[1] == "width" && n > phone {
					t.Errorf("%s: %s", name, tag)
				}
			}
		}
		if strings.Contains(src, "<iframe") {
			t.Errorf("%s: an iframe is a fixed-size surprise on a phone", name)
		}
	}
	all, _ := loadCSS(t)
	eff := effective(all, phone)
	// Long identifiers (IRNs, URLs, keys) must wrap rather than widen the page.
	wraps := false
	for sel, d := range eff {
		if (sel == "code" || sel == "code.wrap" || strings.HasSuffix(sel, " code")) && strings.Contains(d["overflow-wrap"]+d["word-break"], "anywhere") || d["word-break"] == "break-all" {
			wraps = true
		}
	}
	if !wraps {
		t.Error("code and identifiers need overflow-wrap: anywhere")
	}
	if v := eff["pre"]["overflow"]; v != "auto" && v != "scroll" {
		t.Errorf("pre overflow is %q: code blocks must scroll inside themselves", v)
	}
}

func TestLayoutDeclaresAViewportAndNoLaterPageUndoesIt(t *testing.T) {
	layout := templates(t)["layout.html"]
	if !strings.Contains(layout, `name="viewport" content="width=device-width, initial-scale=1"`) {
		t.Error("layout.html has no responsive viewport meta tag")
	}
	for name, src := range templates(t) {
		if strings.Contains(src, `name="viewport"`) && name != "layout.html" {
			t.Errorf("%s overrides the viewport", name)
		}
		if strings.Contains(src, "user-scalable=no") || strings.Contains(src, "maximum-scale=1") {
			t.Errorf("%s stops people zooming", name)
		}
	}
}
