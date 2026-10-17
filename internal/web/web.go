// Package web serves templates and static assets from the embedded defaults
// overlaid by an optional custom directory (PAYMUX_DATA_DIR/custom). This
// overlay is the theming seam: a theme is just files that shadow defaults.
package web

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sort"

	webassets "github.com/paymux/paymux/web"
)

// Embedded returns the built-in assets.
func Embedded() fs.FS { return webassets.FS }

// Overlay is an fs.FS where files in Custom shadow files in Theme, which
// shadow files in Base: embedded -> active theme -> custom/.
type Overlay struct {
	Base   fs.FS
	Theme  fs.FS // nil when no theme is active
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
	for _, layer := range []fs.FS{o.Custom, o.Theme} {
		if layer == nil {
			continue
		}
		if f, err := layer.Open(name); err == nil {
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
	for _, f := range []fs.FS{o.Base, o.Theme, o.Custom} {
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

// Field is a hidden form field for form_post checkouts.
type Field struct{ Name, Value string }

// StateOption is one entry of the India state dropdown.
type StateOption struct{ Code, Name string }

// Billing holds the pay-page billing form values.
type Billing struct {
	Name, Email, StateCode, Line1, PostalCode, GSTIN, LegalName, Country string
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
	// payment link page
	Link *LinkForm
	// the simulator's hosted page
	Sim *SimPage
}

// SimPage is what the simulator's hosted checkout page shows.
type SimPage struct {
	Kind, Status, Currency, Amount string
	// Action is where the buttons post; ReturnURL leads back to the merchant.
	Action, ReturnURL string
	// Open: the customer can still choose. Authorized: a capture is waiting.
	// Waiting: a delayed success is on its way. Done: nothing left to choose.
	Open, Authorized, Waiting, Done, Late bool
}

// LinkForm is what the payment link page shows and keeps between attempts.
type LinkForm struct {
	Description string
	// FixedAmount is the price ("INR 499.00") of a fixed-price link.
	FixedAmount string
	// Choose is set when the customer enters the amount; Currency, Hint and
	// Amount (as typed) belong to that input.
	Choose             bool
	Currency, Hint     string
	Amount             string
	Name, Email, Phone string
}

// AssetHandler serves static/ from fsys under /assets/.
func AssetHandler(fsys fs.FS) http.Handler {
	static, err := fs.Sub(fsys, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/assets/", http.FileServerFS(static))
}
