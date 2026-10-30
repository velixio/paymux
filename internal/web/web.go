// Package web serves templates and static assets from the embedded defaults
// overlaid by an optional custom directory (PAYMUX_DATA_DIR/custom). This
// overlay is the theming seam: a theme is just files that shadow defaults.
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/velixio/paymux/pkg/connector"
	webassets "github.com/velixio/paymux/web"
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
	// Nonce is the CSP nonce of this response; the page's own scripts carry it.
	Nonce string
	// form_post
	FormURL string
	Fields  []Field
	// sdk
	ScriptURL string
	Launcher  string
	// Hosts are the hosts the connector account declared; the page's policy
	// allows them and nothing else.
	Hosts   connector.CheckoutHosts
	Options map[string]any
	// billing form
	CSRF                           string
	Error                          string
	Billing                        Billing
	IndiaStates                    []StateOption
	StateRequired, AddressRequired bool
	CountryRequired                bool
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

// hashedAsset matches build output whose name carries a content hash
// (vendor-react-Dph6fTCk.js, inter-latin-…-Dx4kXJAl.woff2): a new build gives
// it a new name, so browsers may keep it for good.
var hashedAsset = regexp.MustCompile(`-[A-Za-z0-9_-]{8}\.(js|css|woff2?)$`)

// AssetHandler serves static/ from fsys under /assets/. Hashed files are
// cached for a year; fixed-name files (main.js, style.css, css/…) carry an
// ETag of their content and must be revalidated, so an upgraded binary is
// picked up at once instead of a browser running an old dashboard.
func AssetHandler(fsys fs.FS) http.Handler {
	static, err := fs.Sub(fsys, "static")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(static)
	return http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// Files only: no directory listings (of the bundle or of the operator's
		// custom/static overlay).
		if st, err := fs.Stat(static, name); r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") || (err == nil && st.IsDir()) {
			http.NotFound(w, r)
			return
		}
		if hashedAsset.MatchString(name) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else if data, err := fs.ReadFile(static, name); err == nil {
			sum := sha256.Sum256(data)
			w.Header().Set("Cache-Control", "no-cache")
			// http.FileServer answers If-None-Match against this header.
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:12])+`"`)
		}
		files.ServeHTTP(w, r)
	}))
}
