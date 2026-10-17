// Package web serves templates and static assets from the embedded defaults
// overlaid by an optional custom directory (PAYMUX_DATA_DIR/custom). This
// overlay is the theming seam: a theme is just files that shadow defaults.
package web

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"

	webassets "github.com/paymux/paymux/web"
)

// Embedded returns the built-in assets.
func Embedded() fs.FS { return webassets.FS }

// Overlay is an fs.FS where files in Custom shadow files in Base.
type Overlay struct {
	Base   fs.FS
	Custom fs.FS // nil when no custom dir exists
}

// NewOverlay opens customDir with os.Root (no escapes via .. or symlinks).
// A missing customDir is not an error.
func NewOverlay(base fs.FS, customDir string) (*Overlay, error) {
	o := &Overlay{Base: base}
	if customDir == "" {
		return o, nil
	}
	root, err := os.OpenRoot(customDir)
	if errors.Is(err, fs.ErrNotExist) {
		return o, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open custom dir %s: %w", customDir, err)
	}
	o.Custom = root.FS()
	return o, nil
}

// Open implements fs.FS.
func (o *Overlay) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if o.Custom != nil {
		if f, err := o.Custom.Open(name); err == nil {
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				return f, nil
			}
			f.Close()
		}
	}
	return o.Base.Open(name)
}

// ReadDir merges directory listings, custom entries taking precedence.
func (o *Overlay) ReadDir(name string) ([]fs.DirEntry, error) {
	seen := map[string]fs.DirEntry{}
	var firstErr error
	for _, f := range []fs.FS{o.Base, o.Custom} {
		if f == nil {
			continue
		}
		es, err := fs.ReadDir(f, name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, e := range es {
			seen[e.Name()] = e
		}
	}
	if len(seen) == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]fs.DirEntry, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Renderer holds one template set per page, each combined with the layout.
type Renderer struct{ pages map[string]*template.Template }

// NewRenderer parses templates/layout.html plus every other templates/*.html page.
func NewRenderer(fsys fs.FS) (*Renderer, error) {
	layout, err := fs.ReadFile(fsys, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: layout: %w", err)
	}
	entries, err := fs.ReadDir(fsys, "templates")
	if err != nil {
		return nil, err
	}
	r := &Renderer{pages: map[string]*template.Template{}}
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
		t, err := template.New("layout").Parse(string(layout))
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
		r.pages[strings.TrimSuffix(n, ".html")] = t
	}
	return r, nil
}

// Render executes page within the layout.
func (r *Renderer) Render(w io.Writer, page string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("web: unknown page %q", page)
	}
	return t.ExecuteTemplate(w, "layout", data)
}

// Field is a hidden form field for form_post checkouts.
type Field struct{ Name, Value string }

// StateOption is one entry of the India state dropdown.
type StateOption struct{ Code, Name string }

// Billing holds the pay-page billing form values.
type Billing struct {
	Name, Email, StateCode, Line1, PostalCode, GSTIN, LegalName string
}

// PayPage is the view model for pay_* templates.
type PayPage struct {
	AppName, LogoURL, Accent string
	// form_post
	FormURL string
	Fields  []Field
	// sdk
	ScriptURL string
	Launcher  string
	Options   map[string]any
	// billing form
	CSRF                           string
	Error                          string
	Billing                        Billing
	IndiaStates                    []StateOption
	StateRequired, AddressRequired bool
	// result
	Title, Message, ReturnURL string
}

// AssetHandler serves static/ from fsys under /assets/.
func AssetHandler(fsys fs.FS) http.Handler {
	static, err := fs.Sub(fsys, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/assets/", http.FileServerFS(static))
}
