package web

import (
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
)

// Renderer renders pages within the layout. A Renderer made by NewThemedRenderer
// can render the same page in any installed theme (and, with the catalogs,
// any language): With returns a view bound to one theme and language.
type Renderer struct {
	reg         *registry
	theme, lang string
}

// NewRenderer parses templates/layout.html plus every other templates/*.html
// page of fsys. It has no themes.
func NewRenderer(fsys fs.FS) (*Renderer, error) {
	reg := &registry{static: fsys, now: time.Now, entries: map[string]*entry{}, alerted: map[string]bool{}}
	if _, err := reg.get("", ""); err != nil {
		return nil, err
	}
	return &Renderer{reg: reg}, nil
}

// ThemedOptions configures NewThemedRenderer.
type ThemedOptions struct {
	Base      fs.FS   // the embedded assets
	CustomDir string  // PAYMUX_DATA_DIR/custom; may be missing
	Themes    *Themes // nil: no themes
	// Alert is told once about each theme that cannot be used (bad theme.json,
	// a template that does not parse); the page then falls back to the
	// embedded look.
	Alert func(theme string, err error)
	// Recheck is how often a theme's files are checked for edits (default 1s;
	// negative: on every render).
	Recheck time.Duration
	Now     func() time.Time
}

// NewThemedRenderer builds a Renderer over embedded -> theme -> custom/.
func NewThemedRenderer(o ThemedOptions) (*Renderer, error) {
	plain, err := NewOverlay(o.Base, o.CustomDir)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Recheck == 0 {
		o.Recheck = time.Second
	}
	reg := &registry{base: plain.Base, custom: plain.Custom, themes: o.Themes, alert: o.Alert, recheck: o.Recheck, now: o.Now,
		entries: map[string]*entry{}, alerted: map[string]bool{}}
	if _, err := reg.get("", ""); err != nil {
		return nil, err
	}
	return &Renderer{reg: reg}, nil
}

// With returns a Renderer for theme (empty: none) and language (empty: en).
func (r *Renderer) With(theme, lang string) *Renderer {
	return &Renderer{reg: r.reg, theme: theme, lang: lang}
}

// Render executes page within the layout.
func (r *Renderer) Render(w io.Writer, page string, data any) error {
	e, err := r.reg.get(r.theme, r.lang)
	if err != nil {
		return err
	}
	t, ok := e.pages[page]
	if !ok {
		return fmt.Errorf("web: unknown page %q", page)
	}
	return t.ExecuteTemplate(w, "layout", data)
}

type registry struct {
	static fs.FS // NewRenderer: the only source

	base, custom fs.FS
	themes       *Themes
	alert        func(string, error)
	recheck      time.Duration
	now          func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
	alerted map[string]bool
}

type entry struct {
	pages   map[string]*template.Template
	fp      string // fingerprint of what the pages were built from
	checked time.Time
}

func (g *registry) plain() fs.FS {
	if g.static != nil {
		return g.static
	}
	return &Overlay{Base: g.base, Custom: g.custom}
}

func (g *registry) notify(theme string, err error) {
	k := theme + "|" + err.Error()
	if g.alerted[k] {
		return
	}
	g.alerted[k] = true
	if g.alert != nil {
		g.alert(theme, err)
	}
}

func (g *registry) get(theme, lang string) (*entry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.themes == nil {
		theme = ""
	}
	key := theme + "|" + lang
	e := g.entries[key]
	now := g.now()
	if e != nil && (theme == "" || (g.recheck > 0 && now.Sub(e.checked) < g.recheck)) {
		return e, nil
	}
	var th *Theme
	fp := ""
	if theme != "" {
		var err error
		if th, err = g.themes.Load(theme); err != nil {
			g.notify(theme, err)
			th, fp = nil, "err:"+err.Error()
		} else {
			fp = th.Fingerprint
		}
	}
	if e != nil && e.fp == fp {
		e.checked = now
		return e, nil
	}
	var pages map[string]*template.Template
	if th != nil {
		var overlay fs.FS = &Overlay{Base: g.base, Theme: th.FS, Custom: g.custom}
		var err error
		if pages, err = buildPages(overlay, funcsFor("/theme/"+th.Name+"/theme.css?v="+th.Fingerprint, lang)); err != nil {
			g.notify(theme, fmt.Errorf("theme %s: %w", theme, err))
			pages = nil
		}
	}
	if pages == nil {
		var err error
		if pages, err = buildPages(g.plain(), funcsFor("", lang)); err != nil {
			return nil, err
		}
	}
	e = &entry{pages: pages, fp: fp, checked: now}
	g.entries[key] = e
	return e, nil
}

func funcsFor(themeCSS, lang string) template.FuncMap {
	return template.FuncMap{
		"themeCSS": func() string { return themeCSS },
	}
}

// buildPages parses templates/layout.html with every page of fsys.
func buildPages(fsys fs.FS, funcs template.FuncMap) (map[string]*template.Template, error) {
	layout, err := fs.ReadFile(fsys, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: layout: %w", err)
	}
	entries, err := fs.ReadDir(fsys, "templates")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	// Files starting with "_" are partials: their templates are available to
	// every page but they are not pages themselves.
	var partials []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".html") && strings.HasPrefix(n, "_") {
			b, err := fs.ReadFile(fsys, path.Join("templates", n))
			if err != nil {
				return nil, err
			}
			partials = append(partials, string(b))
		}
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".html") || n == "layout.html" || strings.HasPrefix(n, "_") {
			continue
		}
		body, err := fs.ReadFile(fsys, path.Join("templates", n))
		if err != nil {
			return nil, err
		}
		t, err := template.New("layout").Funcs(funcs).Parse(string(layout))
		for _, p := range partials {
			if err != nil {
				break
			}
			_, err = t.Parse(p)
		}
		if err == nil {
			_, err = t.Parse(string(body))
		}
		if err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", n, err)
		}
		pages[strings.TrimSuffix(n, ".html")] = t
	}
	return pages, nil
}
