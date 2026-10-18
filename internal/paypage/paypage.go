// Package paypage serves the customer-facing pages: the pay page (with the
// billing-details form), the provider return endpoint and the static assets.
// Pages carry no cookies; the payment's client secret in the URL
// authenticates them, plus a per-payment CSRF token on the billing form.
package paypage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector"
)

// addressThreshold is Rs 50,000 in paise: from this amount an Indian buyer's
// full address is required for the invoice (spec section 10).
const addressThreshold = 5_000_000

const maxForm = 1 << 20

// Options configures the Handler.
type Options struct {
	Core     *core.Service
	Flow     *checkout.Service
	Settings *settings.Service
	Renderer *web.Renderer
	Assets   http.Handler // serves /assets/
	// CSRFKey keys the billing-form CSRF token (HMAC of payment id and secret).
	CSRFKey []byte
	Logger  *slog.Logger
	Now     func() time.Time
}

// Handler serves the pay, return and asset routes.
type Handler struct{ o Options }

// Mux is the part of an HTTP mux the Handler registers on.
type Mux interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// New builds a Handler.
func New(o Options) *Handler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Handler{o: o}
}

// Register mounts the customer routes on m.
func (h *Handler) Register(m Mux) {
	m.HandleFunc("GET /pay/{id}", withLook(h.pay))
	m.HandleFunc("POST /pay/{id}", withLook(h.billingPost))
	m.HandleFunc("GET /.well-known/paymux/{token}", h.wellKnown)
	m.HandleFunc("GET /return/{pa}/{ref}", withLook(h.ret))
	m.HandleFunc("POST /return/{pa}/{ref}", withLook(h.ret))
	if h.o.Assets != nil {
		m.HandleFunc("GET /assets/", h.o.Assets.ServeHTTP)
	}
}

// page renders a template with hardened headers. Pay URLs carry a secret, so
// they must never leak through Referer or caches.
func (h *Handler) page(w http.ResponseWriter, status int, name string, data web.PayPage) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	var buf bytes.Buffer
	if err := h.renderer(w).Render(&buf, name, data); err != nil {
		h.o.Logger.Error("paypage: render", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// result shows a titled message; titleKey and messageKey are catalog keys.
func (h *Handler) result(w http.ResponseWriter, status int, appName, titleKey, messageKey, returnURL string) {
	msg := h.tr(w, messageKey)
	if id := w.Header().Get("X-Request-Id"); status >= 500 && id != "" {
		msg += " (request id: " + id + ")" // for support: it finds the log row
	}
	h.page(w, status, "pay_result", web.PayPage{AppName: appName, Title: h.tr(w, titleKey), Message: msg, ReturnURL: returnURL})
}

func (h *Handler) notFound(w http.ResponseWriter) {
	h.result(w, http.StatusNotFound, "", "result.link_invalid.title", "result.link_invalid.msg", "")
}

// load authenticates a pay-page request: payment id plus client secret (and,
// on a custom domain, that the payment belongs to the domain's app).
func (h *Handler) load(w http.ResponseWriter, r *http.Request) (store.Payment, string, bool) {
	secret := r.URL.Query().Get("t")
	p, err := h.o.Core.GetPaymentByID(r.Context(), r.PathValue("id"))
	if err != nil || secret == "" || !core.ClientSecretMatches(p, secret) || !h.hostMayServe(r.Context(), p.AppID) {
		h.notFound(w)
		return store.Payment{}, "", false
	}
	setApp(w, p.AppID)
	return p, secret, true
}

func (h *Handler) hostMayServe(ctx context.Context, appID string) bool {
	hostApp, onCustomHost := domains.AppFrom(ctx)
	return !onCustomHost || hostApp == appID
}

func appReturnURL(p store.Payment) string {
	if p.ReturnURL == "" {
		return ""
	}
	u, err := url.Parse(p.ReturnURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("payment_id", p.ID)
	q.Set("status", p.Status)
	u.RawQuery = q.Encode()
	return u.String()
}

// statusPage shows the outcome of a payment that is not (or no longer)
// payable. It reports false when the payment can still be paid.
func (h *Handler) statusPage(w http.ResponseWriter, p store.Payment, appName string) bool {
	back := appReturnURL(p)
	switch state.PaymentStatus(p.Status) {
	case state.PaymentSucceeded:
		h.result(w, http.StatusOK, appName, "result.received.title", "result.received.msg", back)
	case state.PaymentCanceled:
		h.result(w, http.StatusOK, appName, "result.canceled.title", "result.canceled.msg", back)
	case state.PaymentNeedsReview:
		h.result(w, http.StatusOK, appName, "result.review.title", "result.review.msg", back)
	case state.PaymentExpired:
		h.result(w, http.StatusOK, appName, "result.expired.title", "result.expired.msg", back)
	default:
		if h.o.Now().After(p.ExpiresAt) {
			h.result(w, http.StatusOK, appName, "result.expired.title", "result.expired.msg", back)
			return true
		}
		return false
	}
	return true
}

func (h *Handler) appName(ctx context.Context, appID string) string {
	app, err := h.o.Core.GetApp(ctx, appID)
	if err != nil {
		return ""
	}
	return app.Name
}

func (h *Handler) pay(w http.ResponseWriter, r *http.Request) {
	p, secret, ok := h.load(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	appName := h.appName(ctx, p.AppID)
	if h.statusPage(w, p, appName) {
		return
	}
	var cust store.Customer
	if p.CustomerID != "" {
		var err error
		if cust, err = h.o.Core.GetCustomer(ctx, p.CustomerID); err != nil {
			h.serverError(w, appName, err)
			return
		}
	}
	need, err := h.billingNeed(ctx, p, cust)
	if err != nil {
		h.serverError(w, appName, err)
		return
	}
	if need.show {
		h.billingForm(w, http.StatusOK, p, secret, appName, cust, need, "")
		return
	}
	// A reload shows the open attempt's checkout again; ?fresh=1 ("try again",
	// "pay another way") starts a new attempt.
	start := h.o.Flow.StartCheckout
	if r.URL.Query().Get("fresh") == "1" {
		start = h.o.Flow.StartFreshCheckout
	}
	st, err := start(ctx, p.ID)
	switch {
	case err == nil:
		h.render(w, r, appName, st)
	case errors.Is(err, checkout.ErrCheckoutFailed):
		h.result(w, http.StatusBadGateway, appName, "result.unavailable.title",
			"result.unavailable.provider", "")
	case errors.Is(err, core.ErrNoRoute), errors.Is(err, core.ErrCurrencyNotAccepted):
		h.result(w, http.StatusUnprocessableEntity, appName, "result.unavailable.title", "result.unavailable.no_method", "")
	case errors.Is(err, core.ErrInvalidState):
		fresh, gerr := h.o.Core.GetPaymentByID(ctx, p.ID)
		if gerr != nil || !h.statusPage(w, fresh, appName) {
			h.result(w, http.StatusConflict, appName, "result.unavailable.title", "result.unavailable.cannot_pay", "")
		}
	default:
		h.serverError(w, appName, err)
	}
}

func (h *Handler) serverError(w http.ResponseWriter, appName string, err error) {
	h.o.Logger.Error("paypage: request failed", "err", err)
	h.result(w, http.StatusInternalServerError, appName, "result.error.title", "result.error.msg", "")
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, appName string, st checkout.Started) {
	a := st.Action
	switch a.Kind {
	case connector.ActionRedirect:
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, a.URL, http.StatusSeeOther)
	case connector.ActionFormPost:
		fields := make([]web.Field, 0, len(a.Fields))
		for k, v := range a.Fields {
			fields = append(fields, web.Field{Name: k, Value: v})
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
		h.page(w, http.StatusOK, "pay_form_post", web.PayPage{AppName: appName, FormURL: a.URL, Fields: fields})
	case connector.ActionSDK:
		h.page(w, http.StatusOK, "pay_sdk", web.PayPage{AppName: appName, ScriptURL: a.ScriptURL, Launcher: st.Connector, Options: a.Options})
	default:
		h.serverError(w, appName, errors.New("unknown next action "+string(a.Kind)))
	}
}

// ---- billing details ----

type billingNeed struct {
	show, stateRequired, addressRequired bool
}

func (h *Handler) setting(ctx context.Context, appID, key string, dst any) error {
	v, err := h.o.Settings.Resolve(ctx, appID, key)
	if err != nil {
		return err
	}
	return v.Decode(dst)
}

// billingNeed decides what the pay page must ask (spec sections 6.2 and 10).
// state_required only applies while the app issues invoices.
func (h *Handler) billingNeed(ctx context.Context, p store.Payment, c store.Customer) (billingNeed, error) {
	var mode string
	var issue bool
	if err := h.setting(ctx, p.AppID, "billing_details", &mode); err != nil {
		return billingNeed{}, err
	}
	if err := h.setting(ctx, p.AppID, "issue_invoices", &issue); err != nil {
		return billingNeed{}, err
	}
	// The buyer is Indian unless their stored country says otherwise (the API
	// may supply it, or an earlier submission of this form did). A buyer abroad
	// needs no State or PIN: the sale is an export.
	india := p.Currency == "INR" && (c.Billing.Country == "" || strings.EqualFold(c.Billing.Country, "IN"))
	if mode == "state_required" && !issue {
		mode = "off"
	}
	n := billingNeed{
		stateRequired:   india && (mode == "state_required" || mode == "required"),
		addressRequired: india && (mode == "required" || issue && mode != "off" && p.Amount >= addressThreshold),
	}
	b := c.Billing
	n.show = n.stateRequired && b.StateCode == "" ||
		n.addressRequired && (b.Line1 == "" || b.PostalCode == "") ||
		mode == "required" && c.Name == "" ||
		mode == "optional" && p.CustomerID == ""
	return n, nil
}

func (h *Handler) csrf(paymentID, secret string) string {
	m := hmac.New(sha256.New, h.o.CSRFKey)
	m.Write([]byte("paymux-pay-csrf|" + paymentID + "|" + secret))
	return hex.EncodeToString(m.Sum(nil))
}

func (h *Handler) billingForm(w http.ResponseWriter, status int, p store.Payment, secret, appName string, c store.Customer, n billingNeed, msg string) {
	data := web.PayPage{AppName: appName, FormURL: "/pay/" + p.ID + "?t=" + url.QueryEscape(secret) + h.langQuery(w), CSRF: h.csrf(p.ID, secret),
		Error: h.tr(w, msg), StateRequired: n.stateRequired, AddressRequired: n.addressRequired,
		Billing: web.Billing{Name: c.Name, Email: c.Email, StateCode: c.Billing.StateCode, Line1: c.Billing.Line1,
			PostalCode: c.Billing.PostalCode, GSTIN: c.Billing.GSTIN, LegalName: c.Billing.LegalName, Country: c.Billing.Country}}
	if data.Billing.Country == "" && p.Currency == "INR" {
		data.Billing.Country = "IN"
	}
	if p.Currency == "INR" {
		codes := tax.StateCodes()
		keys := make([]string, 0, len(codes))
		for code := range codes {
			keys = append(keys, code)
		}
		sort.Strings(keys)
		for _, code := range keys {
			data.IndiaStates = append(data.IndiaStates, web.StateOption{Code: code, Name: codes[code]})
		}
	}
	h.page(w, status, "pay_billing", data)
}

var pinRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// countryRE is an ISO 3166 alpha-2 code, upper case.
var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

func (h *Handler) billingPost(w http.ResponseWriter, r *http.Request) {
	p, secret, ok := h.load(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	appName := h.appName(ctx, p.AppID)
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		h.result(w, http.StatusBadRequest, appName, "result.invalid.title", "result.invalid.form", "")
		return
	}
	if !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(h.csrf(p.ID, secret))) {
		h.result(w, http.StatusForbidden, appName, "result.session.title", "result.session.msg", "")
		return
	}
	if h.statusPage(w, p, appName) {
		return
	}
	var cust store.Customer
	if p.CustomerID != "" {
		var err error
		if cust, err = h.o.Core.GetCustomer(ctx, p.CustomerID); err != nil {
			h.serverError(w, appName, err)
			return
		}
	}
	need, err := h.billingNeed(ctx, p, cust)
	if err != nil {
		h.serverError(w, appName, err)
		return
	}
	in, msg := h.parseBilling(r, p, need)
	if msg != "" {
		// Re-show what the buyer typed.
		cust.Name, cust.Email = in.Name, in.Email
		cust.Billing = in.Billing
		h.billingForm(w, http.StatusBadRequest, p, secret, appName, cust, need, msg)
		return
	}
	if err := h.o.Core.SetBilling(ctx, p.ID, in); err != nil {
		h.serverError(w, appName, err)
		return
	}
	http.Redirect(w, r, "/pay/"+p.ID+"?t="+url.QueryEscape(secret)+h.langQuery(w), http.StatusSeeOther)
}

func (h *Handler) parseBilling(r *http.Request, p store.Payment, need billingNeed) (core.BillingInput, string) {
	f := r.PostForm
	get := func(k string) string { return strings.TrimSpace(f.Get(k)) }
	in := core.BillingInput{Name: get("name"), Email: get("email")}
	b := &in.Billing
	b.StateCode, b.Line1, b.PostalCode = get("state_code"), get("line1"), get("postal_code")
	b.GSTIN, b.LegalName = strings.ToUpper(get("gstin")), get("legal_name")
	if len(in.Name) > 200 || len(b.Line1) > 200 || len(b.LegalName) > 200 {
		return in, "billing.err.too_long"
	}
	if in.Name == "" {
		return in, "billing.err.name"
	}
	if in.Email != "" {
		if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email {
			return in, "billing.err.email"
		}
	}
	b.Country = strings.ToUpper(get("country"))
	if b.Country == "" && p.Currency == "INR" {
		b.Country = "IN"
	}
	if b.Country != "" && !countryRE.MatchString(b.Country) {
		return in, "billing.err.country"
	}
	if b.Country != "" && b.Country != "IN" {
		// A buyer abroad: an export. No Indian State, address check or GSTIN applies.
		b.StateCode, b.GSTIN, b.LegalName = "", "", ""
		return in, ""
	}
	if b.StateCode != "" {
		if _, ok := tax.StateName(b.StateCode); !ok {
			return in, "billing.err.state_invalid"
		}
	}
	if need.stateRequired && b.StateCode == "" && b.GSTIN == "" {
		return in, "billing.err.state_required"
	}
	if b.GSTIN != "" {
		if err := tax.ValidGSTIN(b.GSTIN); err != nil {
			return in, "billing.err.gstin"
		}
		if b.LegalName == "" {
			return in, "billing.err.legal_name"
		}
		gst := tax.StateFromGSTIN(b.GSTIN)
		switch {
		case b.StateCode == "":
			b.StateCode = gst
		case b.StateCode != gst:
			return in, "billing.err.gst_state"
		}
	}
	if need.stateRequired && b.StateCode == "" {
		return in, "billing.err.state_required"
	}
	if need.addressRequired {
		if b.Line1 == "" {
			return in, "billing.err.address"
		}
		if !pinRE.MatchString(b.PostalCode) {
			return in, "billing.err.pin"
		}
	}
	return in, ""
}

// ---- return ----

func (h *Handler) ret(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		h.result(w, http.StatusBadRequest, "", "result.invalid.title", "result.invalid.request", "")
		return
	}
	// Returns from subscription and mandate checkouts are not payment attempts.
	if back, handled, err := h.o.Flow.SubscriptionReturn(ctx, r.PathValue("pa"), r.PathValue("ref")); handled {
		switch {
		case err != nil:
			h.serverError(w, "", err)
		case back != "":
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, back, http.StatusSeeOther)
		default:
			h.result(w, http.StatusOK, "", "result.thanks.title", "result.thanks.subscription", "")
		}
		return
	} else if err != nil {
		h.serverError(w, "", err)
		return
	}
	p, err := h.o.Flow.HandleReturn(ctx, r.PathValue("pa"), r.PathValue("ref"), checkout.ReturnInput{
		Method: r.Method, Query: r.URL.Query(), Form: r.PostForm, Headers: r.Header})
	if errors.Is(err, core.ErrNotFound) || err == nil && !h.hostMayServe(ctx, p.AppID) {
		h.notFound(w)
		return
	}
	if err != nil {
		h.serverError(w, "", err)
		return
	}
	if back := appReturnURL(p); back != "" {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	setApp(w, p.AppID)
	appName := h.appName(ctx, p.AppID)
	if h.statusPage(w, p, appName) {
		return
	}
	h.result(w, http.StatusOK, appName, "result.confirming.title",
		"result.confirming.msg", "")
}

type stdMux struct{ m *http.ServeMux }

func (s stdMux) HandleFunc(pattern string, fn http.HandlerFunc) { s.m.HandleFunc(pattern, fn) }

// StdMux adapts an *http.ServeMux (for example the one serving custom
// domains) to Mux.
func StdMux(m *http.ServeMux) Mux { return stdMux{m} }

// wellKnown serves a custom domain's activation token so the background check
// can prove the domain points at this server. Only a registered custom host
// (served through the host router) with the right token answers.
func (h *Handler) wellKnown(w http.ResponseWriter, r *http.Request) {
	if !h.o.Core.VerifyDomainToken(r.Context(), r.Host, r.PathValue("token")) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(r.PathValue("token")))
}
