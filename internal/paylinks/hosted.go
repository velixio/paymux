package paylinks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/look"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector/money"
)

const (
	visitorCookie = "pm_visitor"
	maxFormBytes  = 64 << 10
)

// HostedOptions configures the public /l/{slug} pages.
type HostedOptions struct {
	Service  *Service
	Core     *core.Service // app names, and the domain a request came in on
	Renderer *web.Renderer
	Settings look.Settings
	// CSRFKey keys the form token (HMAC of the link and the visitor cookie).
	CSRFKey []byte
	// Secure marks the visitor cookie Secure (https base URL).
	Secure bool
	Logger *slog.Logger
	Now    func() time.Time
}

// Hosted serves a link's page: a small form that creates the payment and
// sends the customer on to /pay/{id}.
type Hosted struct{ o HostedOptions }

// NewHosted builds the page handlers.
func NewHosted(o HostedOptions) *Hosted {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Hosted{o: o}
}

// visitor returns the browser's visitor id, setting the cookie when it has none.
func (h *Hosted) visitor(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(visitorCookie); err == nil && len(c.Value) >= 20 && len(c.Value) <= 64 {
		return c.Value
	}
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic("paylinks: entropy source failed: " + err.Error())
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: visitorCookie, Value: v, Path: "/l/", HttpOnly: true, Secure: h.o.Secure, SameSite: http.SameSiteLaxMode})
	return v
}

func (h *Hosted) csrf(slug, visitor string) string {
	m := hmac.New(sha256.New, h.o.CSRFKey)
	m.Write([]byte("paymux-link-csrf|" + slug + "|" + visitor))
	return hex.EncodeToString(m.Sum(nil))
}

// pageCtx carries what rendering a page needs.
type pageCtx struct {
	w    http.ResponseWriter
	r    *http.Request
	look look.Look
	rend *web.Renderer
}

func (h *Hosted) newCtx(w http.ResponseWriter, r *http.Request, appID string) *pageCtx {
	lk := look.Resolve(r.Context(), h.o.Settings, h.o.Renderer, appID, r.URL.Query().Get("lang"))
	return &pageCtx{w: w, r: r, look: lk, rend: lk.Bind(h.o.Renderer)}
}

func (c *pageCtx) tr(h *Hosted, key string, args ...any) string {
	return h.o.Renderer.T(c.look.Lang, key, args...)
}

// langQuery keeps a ?lang= chosen by the customer on the form's action and redirects.
func (h *Hosted) langQuery(r *http.Request) string {
	if q := r.URL.Query().Get("lang"); q != "" && h.o.Renderer.HasLang(q) {
		return "&lang=" + url.QueryEscape(q)
	}
	return ""
}

func (h *Hosted) write(c *pageCtx, status int, name string, data web.PayPage) {
	hdr := c.w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	web.SetHostedCSP(hdr, &data)
	var buf bytes.Buffer
	if err := c.rend.Render(&buf, name, data); err != nil {
		h.o.Logger.Error("paylinks: render", "page", name, "err", err)
		http.Error(c.w, "internal error", http.StatusInternalServerError)
		return
	}
	c.w.WriteHeader(status)
	_, _ = c.w.Write(buf.Bytes())
}

func (h *Hosted) result(c *pageCtx, status int, appName, titleKey, msgKey string) {
	h.write(c, status, "pay_result", web.PayPage{AppName: appName, Title: c.tr(h, titleKey), Message: c.tr(h, msgKey)})
}

// load finds the link and checks that this host may serve it (a custom
// domain serves only its own app's links). It writes the error page itself.
func (h *Hosted) load(w http.ResponseWriter, r *http.Request) (store.PaymentLink, *pageCtx, string, bool) {
	l, err := h.o.Service.bySlug(r.Context(), r.PathValue("slug"))
	if err == nil {
		if hostApp, onCustom := domains.AppFrom(r.Context()); onCustom && hostApp != l.AppID {
			err = newErr(ErrNotFound, "payment link not found")
		}
	}
	if errors.Is(err, ErrNotFound) {
		c := h.newCtx(w, r, "")
		h.result(c, http.StatusNotFound, "", "result.link_invalid.title", "result.link_invalid.msg")
		return store.PaymentLink{}, nil, "", false
	}
	if err != nil {
		h.o.Logger.Error("paylinks: loading a link", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.PaymentLink{}, nil, "", false
	}
	c := h.newCtx(w, r, l.AppID)
	app := h.appName(r.Context(), l.AppID)
	if err := h.o.Service.checkState(l); err != nil {
		h.stateResult(c, app, err)
		return store.PaymentLink{}, nil, "", false
	}
	return l, c, app, true
}

func (h *Hosted) appName(ctx context.Context, appID string) string {
	if h.o.Core == nil {
		return ""
	}
	a, err := h.o.Core.GetApp(ctx, appID)
	if err != nil {
		return ""
	}
	return a.Name
}

// stateResult shows why a link cannot take a payment.
func (h *Hosted) stateResult(c *pageCtx, app string, err error) {
	switch {
	case errors.Is(err, ErrExpired):
		h.result(c, http.StatusGone, app, "link.expired.title", "link.expired.msg")
	case errors.Is(err, ErrInactive):
		h.result(c, http.StatusGone, app, "link.inactive.title", "link.inactive.msg")
	case errors.Is(err, ErrBusy):
		h.result(c, http.StatusConflict, app, "link.busy.title", "link.busy.msg")
	case errors.Is(err, ErrRateLimited):
		c.w.Header().Set("Retry-After", "60")
		h.result(c, http.StatusTooManyRequests, app, "link.limited.title", "link.limited.msg")
	case errors.Is(err, ErrUnavailable):
		h.result(c, http.StatusUnprocessableEntity, app, "result.unavailable.title", "result.unavailable.no_method")
	case errors.Is(err, ErrNotFound):
		h.result(c, http.StatusNotFound, app, "result.link_invalid.title", "result.link_invalid.msg")
	default:
		h.o.Logger.Error("paylinks: starting a payment", "err", err)
		h.result(c, http.StatusInternalServerError, app, "result.error.title", "result.error.msg")
	}
}

func (h *Hosted) form(c *pageCtx, l store.PaymentLink, app string, status int, fv web.LinkForm, msg, visitor string) {
	fv.Description = l.Description
	if l.Amount > 0 {
		fv.FixedAmount = h.o.Service.format(l.Amount, l.Currency)
	} else {
		fv.Choose, fv.Currency = true, l.Currency
		lo := h.o.Service.format(l.MinAmount, l.Currency)
		if l.MaxAmount > 0 {
			fv.Hint = c.tr(h, "link.hint_range", lo, h.o.Service.format(l.MaxAmount, l.Currency))
		} else {
			fv.Hint = c.tr(h, "link.hint_min", lo)
		}
	}
	h.write(c, status, "pay_link", web.PayPage{AppName: app, FormURL: "/l/" + l.Slug + queryOf(c.r), CSRF: h.csrf(l.Slug, visitor),
		Error: msg, Link: &fv})
}

// queryOf is "?lang=xx" when the request chose a language, for the form action.
func queryOf(r *http.Request) string {
	if q := r.URL.Query().Get("lang"); q != "" {
		return "?lang=" + url.QueryEscape(q)
	}
	return ""
}

// Show serves GET /l/{slug}.
func (h *Hosted) Show(w http.ResponseWriter, r *http.Request) {
	l, c, app, ok := h.load(w, r)
	if !ok {
		return
	}
	h.form(c, l, app, http.StatusOK, web.LinkForm{}, "", h.visitor(w, r))
}

// parseAmount reads what a customer typed: "1,250.50", "499", " 499.00 ".
func parseAmount(s, currency string) (int64, bool) {
	s = strings.NewReplacer(",", "", " ", "").Replace(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	n, err := money.ParseDecimal(s, currency)
	return n, err == nil && n > 0
}

// Submit serves POST /l/{slug}.
func (h *Hosted) Submit(w http.ResponseWriter, r *http.Request) {
	l, c, app, ok := h.load(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		h.result(c, http.StatusBadRequest, app, "result.invalid.title", "result.invalid.form")
		return
	}
	cookie, err := r.Cookie(visitorCookie)
	if err != nil || !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(h.csrf(l.Slug, cookie.Value))) {
		h.result(c, http.StatusForbidden, app, "result.session.title", "result.session.msg")
		return
	}
	f := r.PostForm
	fv := web.LinkForm{Name: strings.TrimSpace(f.Get("name")), Email: strings.TrimSpace(f.Get("email")), Phone: strings.TrimSpace(f.Get("phone")),
		Amount: strings.TrimSpace(f.Get("amount"))}
	in := StartInput{Name: fv.Name, Email: fv.Email, Phone: fv.Phone, Holder: cookie.Value}
	if l.Amount == 0 {
		amount, ok := parseAmount(fv.Amount, l.Currency)
		if !ok {
			h.form(c, l, app, http.StatusBadRequest, fv, c.tr(h, "link.err.amount_format"), cookie.Value)
			return
		}
		in.Amount = amount
	}
	started, err := h.o.Service.Start(r.Context(), l.Slug, in)
	if err != nil {
		var le *Error
		if errors.As(err, &le) && errors.Is(err, ErrInvalid) {
			msg := le.Message
			if le.Key != "" {
				msg = c.tr(h, le.Key, le.Args...)
			}
			h.form(c, l, app, http.StatusBadRequest, fv, msg, cookie.Value)
			return
		}
		h.stateResult(c, app, err)
		return
	}
	// The secret in this URL opens the payment: it must not leak or be cached.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/pay/"+started.PaymentID+"?t="+url.QueryEscape(started.ClientSecret)+h.langQuery(r), http.StatusSeeOther)
}
