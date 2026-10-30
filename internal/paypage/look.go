package paypage

import (
	"net/http"
	"net/url"

	"github.com/velixio/paymux/internal/look"
	"github.com/velixio/paymux/internal/web"
)

// A request's theme and language depend on its app, which a handler only
// learns after it has loaded the payment. lookWriter carries the request and
// that app id along with the ResponseWriter, so every page helper can draw
// with the right look without each call site passing it through.
type lookWriter struct {
	http.ResponseWriter
	r     *http.Request
	appID string
	look  *look.Look
}

// withLook wraps a handler so that its pages are themed and translated.
func withLook(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(&lookWriter{ResponseWriter: w, r: r}, r)
	}
}

// setApp tells w which app the request is for; it is the source of the theme
// and customer_language settings.
func setApp(w http.ResponseWriter, appID string) {
	if lw, ok := w.(*lookWriter); ok {
		lw.appID, lw.look = appID, nil
	}
}

func (h *Handler) lookOf(w http.ResponseWriter) look.Look {
	lw, ok := w.(*lookWriter)
	if !ok {
		return look.Look{Theme: "default", Lang: "en"}
	}
	if lw.look == nil {
		l := look.Resolve(lw.r.Context(), h.o.Settings, h.o.Renderer, lw.appID, lw.r.URL.Query().Get("lang"))
		lw.look = &l
	}
	return *lw.look
}

func (h *Handler) renderer(w http.ResponseWriter) *web.Renderer {
	return h.lookOf(w).Bind(h.o.Renderer)
}

// tr translates a catalog key for the request's language ("" stays empty).
func (h *Handler) tr(w http.ResponseWriter, key string, args ...any) string {
	if key == "" {
		return ""
	}
	return h.o.Renderer.T(h.lookOf(w).Lang, key, args...)
}

// langQuery is "&lang=xx" when this request chose its language with ?lang=, so
// that links and forms of the page keep it.
func (h *Handler) langQuery(w http.ResponseWriter) string {
	lw, ok := w.(*lookWriter)
	if !ok {
		return ""
	}
	if q := lw.r.URL.Query().Get("lang"); q != "" && h.o.Renderer.HasLang(q) {
		return "&lang=" + url.QueryEscape(q)
	}
	return ""
}
