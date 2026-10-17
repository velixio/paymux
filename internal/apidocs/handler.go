package apidocs

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/paymux/paymux/internal/look"
	"github.com/paymux/paymux/internal/web"
)

// Options configures the Handler.
type Options struct {
	Spec     []byte // the contents of docs/openapi.yaml
	Renderer *web.Renderer
	// Settings, when set, gives the page the org's theme.
	Settings look.Settings
	Logger   *slog.Logger
}

// Handler serves the reference page and the raw spec.
type Handler struct {
	o    Options
	page Page

	mu    sync.Mutex
	cache map[string][]byte // rendered page per theme
}

// NewHandler parses the spec once; a spec that does not parse is an error at
// startup, not a broken page later.
func NewHandler(o Options) (*Handler, error) {
	if o.Renderer == nil {
		return nil, errors.New("apidocs: a Renderer is required")
	}
	doc, err := Parse(o.Spec)
	if err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Handler{o: o, page: doc.Page(), cache: map[string][]byte{}}, nil
}

func secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("Cache-Control", "public, max-age=300")
}

func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// Page serves GET /docs.
func (h *Handler) Page(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	theme := "default"
	if h.o.Settings != nil {
		theme = look.Dashboard(r.Context(), h.o.Settings)
	}
	h.mu.Lock()
	body, ok := h.cache[theme]
	if !ok {
		var buf bytes.Buffer
		if err := h.o.Renderer.With(theme, "en").Render(&buf, "docs", h.page); err != nil {
			h.mu.Unlock()
			h.o.Logger.Error("apidocs: render", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		body = buf.Bytes()
		h.cache[theme] = body
	}
	h.mu.Unlock()
	secure(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// Raw serves GET /docs/openapi.yaml.
func (h *Handler) Raw(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	secure(w)
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(h.o.Spec)
	}
}
