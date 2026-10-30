// Package httpapi serves the public /v1 JSON API: Bearer API-key
// authentication, Idempotency-Key replay for POSTs, a uniform error format,
// and the payment and refund routes.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/invoice"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/paylinks"
	"github.com/velixio/paymux/internal/payouts"
	"github.com/velixio/paymux/internal/redact"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/server"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/subscriptions"
	"github.com/velixio/paymux/pkg/connector"
)

const maxBody = 1 << 20 // 1 MiB

// Options configures the API.
type Options struct {
	Core *core.Service
	DB   *store.DB
	// Checkout resolves provider actions for merchant backends. When nil, the
	// provider-action endpoint is unavailable.
	Checkout *checkout.Service
	// Settings selects each app's checkout mode. Nil keeps the legacy hosted
	// behavior for embedders that only use a subset of the API.
	Settings *settings.Service
	// Box seals stored idempotent responses (they contain checkout secrets).
	Box *secrets.Box
	// Invoices serves /v1/invoices and /v1/credit_notes; nil answers 404.
	Invoices *invoice.Service
	// Subscriptions serves /v1/subscriptions; nil answers 404.
	Subscriptions *subscriptions.Service
	// PaymentLinks serves /v1/payment_links; nil answers 404.
	PaymentLinks *paylinks.Service
	// Payouts serves /v1/payouts; nil answers 404.
	Payouts *payouts.Service
	// BaseURL is PAYMUX_BASE_URL, the origin of checkout_url.
	BaseURL *url.URL
	Log     *slog.Logger
	// Redact scrubs a provider's message before it is put in an error response
	// (the hub passes its log redactor, which knows every account's secrets). Nil
	// scrubs by pattern only.
	Redact func(string) string
	// RatePerSecond is the request rate allowed per API key (default 20; a
	// negative value disables the limit). RateBurst is the bucket size
	// (default: the rate rounded up). Now is the clock (tests).
	RatePerSecond float64
	RateBurst     int
	Now           func() time.Time
}

// RouteScopes is the API key scope each route requires: "x:write" for routes
// that change state and "x:read" for reads (a key with "x:write" can also read
// "x"). docs/openapi.yaml documents the same value per operation as
// x-required-scope, and a test keeps the two in step.
var RouteScopes = map[string]string{
	"POST /v1/payments":                          authz.ScopePaymentsWrite,
	"POST /v1/payments/{id}/checkout-action":     authz.ScopePaymentsWrite,
	"GET /v1/payments/{id}":                      authz.ScopePaymentsRead,
	"GET /v1/payments":                           authz.ScopePaymentsRead,
	"POST /v1/payments/{id}/cancel":              authz.ScopePaymentsWrite,
	"POST /v1/payments/{id}/refunds":             authz.ScopeRefundsWrite,
	"GET /v1/refunds/{id}":                       authz.ScopeRefundsRead,
	"POST /v1/subscriptions":                     authz.ScopeSubscriptionsWrite,
	"GET /v1/subscriptions/{id}":                 authz.ScopeSubscriptionsRead,
	"GET /v1/subscriptions":                      authz.ScopeSubscriptionsRead,
	"POST /v1/subscriptions/{id}/cancel":         authz.ScopeSubscriptionsWrite,
	"POST /v1/subscriptions/{id}/update":         authz.ScopeSubscriptionsWrite,
	"POST /v1/subscriptions/{id}/payment_method": authz.ScopeSubscriptionsWrite,
	"POST /v1/payment_links":                     authz.ScopePaymentsWrite,
	"GET /v1/payment_links/{id}":                 authz.ScopePaymentsRead,
	"GET /v1/payment_links":                      authz.ScopePaymentsRead,
	"POST /v1/payment_links/{id}/deactivate":     authz.ScopePaymentsWrite,
	"GET /v1/invoices/{id}":                      authz.ScopeInvoicesRead,
	"GET /v1/invoices/{id}/pdf":                  authz.ScopeInvoicesRead,
	"GET /v1/invoices":                           authz.ScopeInvoicesRead,
	"GET /v1/credit_notes/{id}":                  authz.ScopeInvoicesRead,
	"GET /v1/credit_notes/{id}/pdf":              authz.ScopeInvoicesRead,
	"GET /v1/credit_notes":                       authz.ScopeInvoicesRead,
	"POST /v1/payouts":                           authz.ScopePayoutsWrite,
	"GET /v1/payouts/{id}":                       authz.ScopePayoutsRead,
	"GET /v1/payouts":                            authz.ScopePayoutsRead,
}

// Mux is the part of server.Server the API needs.
type Mux interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// API holds the handlers.
type API struct {
	core     *core.Service
	db       *store.DB
	checkout *checkout.Service
	settings *settings.Service
	now      func() time.Time
	box      *secrets.Box
	baseURL  string
	log      *slog.Logger
	limit    *limiter // nil: unlimited
	// redact scrubs a provider's message before it is shown to the caller (the
	// hub's log redactor, which knows every account's secrets); nil: patterns only.
	redact func(string) string

	invoices *invoice.Service
	subs     *subscriptions.Service
	links    *paylinks.Service
	payouts  *payouts.Service
}

// New builds the API.
func New(o Options) (*API, error) {
	if o.Core == nil || o.DB == nil || o.Box == nil || o.BaseURL == nil {
		return nil, errors.New("httpapi: Core, DB, Box and BaseURL are required")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	a := &API{redact: o.Redact, core: o.Core, db: o.DB, box: o.Box, checkout: o.Checkout, settings: o.Settings,
		now: o.Now, baseURL: strings.TrimRight(o.BaseURL.String(), "/"), log: o.Log, invoices: o.Invoices, subs: o.Subscriptions, links: o.PaymentLinks, payouts: o.Payouts}
	if o.RatePerSecond >= 0 {
		a.limit = newLimiter(o.RatePerSecond, o.RateBurst, o.Now)
	}
	return a, nil
}

// handler is an authenticated route handler. body is the already-read
// request body (nil for GET).
type handler func(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte)

// Register mounts the /v1 routes on m.
func (a *API) Register(m Mux) {
	route := func(pattern string, h handler, idempotent bool) {
		scope, ok := RouteScopes[pattern]
		if !ok {
			panic("httpapi: no scope declared for " + pattern)
		}
		m.HandleFunc(pattern, a.wrap(h, idempotent, scope))
	}
	route("POST /v1/payments", a.createPayment, true)
	route("POST /v1/payments/{id}/checkout-action", a.createCheckoutAction, true)
	route("GET /v1/payments/{id}", a.getPayment, false)
	route("GET /v1/payments", a.listPayments, false)
	route("POST /v1/payments/{id}/cancel", a.cancelPayment, true)
	route("POST /v1/payments/{id}/refunds", a.createRefund, true)
	route("GET /v1/refunds/{id}", a.getRefund, false)
	route("POST /v1/subscriptions", a.createSubscription, true)
	route("GET /v1/subscriptions/{id}", a.getSubscription, false)
	route("GET /v1/subscriptions", a.listSubscriptions, false)
	route("POST /v1/subscriptions/{id}/cancel", a.cancelSubscription, true)
	route("POST /v1/subscriptions/{id}/update", a.updateSubscription, true)
	route("POST /v1/subscriptions/{id}/payment_method", a.subscriptionPaymentMethod, true)
	route("POST /v1/payment_links", a.createPaymentLink, true)
	route("GET /v1/payment_links/{id}", a.getPaymentLink, false)
	route("GET /v1/payment_links", a.listPaymentLinks, false)
	route("POST /v1/payment_links/{id}/deactivate", a.deactivatePaymentLink, true)
	route("GET /v1/invoices/{id}", a.getInvoice, false)
	route("GET /v1/invoices/{id}/pdf", a.getInvoicePDF, false)
	route("GET /v1/invoices", a.listInvoices, false)
	route("GET /v1/credit_notes/{id}", a.getCreditNote, false)
	route("GET /v1/credit_notes/{id}/pdf", a.getCreditNotePDF, false)
	route("GET /v1/credit_notes", a.listCreditNotes, false)
	route("POST /v1/payouts", a.createPayout, true)
	route("GET /v1/payouts/{id}", a.getPayout, false)
	route("GET /v1/payouts", a.listPayouts, false)
	m.HandleFunc("/v1/", a.wrap(func(w http.ResponseWriter, r *http.Request, _ core.Principal, _ []byte) {
		a.fail(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	}, false, ""))
}

// wrap authenticates, reads the body of POSTs and applies idempotency.
//
// scope is the API key scope the route needs ("" for none). A key limited to
// other scopes gets 403 insufficient_scope; keys made before scopes existed
// have all of them.
func (a *API) wrap(h handler, idempotent bool, scope string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pr, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		noteRequest(r, pr)
		if !authz.ScopeAllows(pr.Scopes, scope) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="paymux", error="insufficient_scope", scope="`+scope+`"`)
			a.fail(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the "+scope+" scope")
			return
		}
		if a.limit != nil {
			if allowed, wait := a.limit.allow(pr.KeyID); !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
				a.fail(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests for this API key; retry after the Retry-After seconds")
				return
			}
		}
		var body []byte
		if r.Method == http.MethodPost {
			var err error
			body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					a.fail(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "request body too large")
				} else {
					a.fail(w, r, http.StatusBadRequest, "invalid_request", "cannot read request body")
				}
				return
			}
		}
		if scope == authz.ScopePayoutsWrite && r.Method == http.MethodPost && r.Header.Get("Idempotency-Key") == "" {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "an Idempotency-Key header is required to create a payout, so that a retry cannot pay twice")
			return
		}
		if idempotent && r.Header.Get("Idempotency-Key") != "" {
			a.idempotent(w, r, pr, body, h)
			return
		}
		h(w, r, pr, body)
	}
}

var ourID = regexp.MustCompile(`^[a-z]{2,5}_[0-9a-z]{26}$`)

// noteRequest tells the request's log scope who is asking and which object the
// path names, so that a failure row, or a provider call made while serving the
// request, can be traced back. The path value is noted only when it looks like
// one of our ids.
func noteRequest(r *http.Request, pr core.Principal) {
	logctx.Note(r.Context(), "org_id", pr.OrgID, "app_id", pr.AppID)
	id := r.PathValue("id")
	if !ourID.MatchString(id) {
		return
	}
	for seg, key := range map[string]string{"/v1/payments/": "payment_id", "/v1/refunds/": "refund_id", "/v1/subscriptions/": "subscription_id",
		"/v1/payment_links/": "link_id", "/v1/invoices/": "invoice_id", "/v1/credit_notes/": "credit_note_id", "/v1/payouts/": "payout_id"} {
		if strings.Contains(r.Pattern, seg) {
			logctx.Note(r.Context(), key, id)
			return
		}
	}
}

func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (core.Principal, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		a.unauthenticated(w, r, "missing or malformed Authorization header; use Bearer pmx_{test|live}_sk_...")
		return core.Principal{}, false
	}
	pr, err := a.core.Authenticate(r.Context(), strings.TrimSpace(token))
	switch {
	case errors.Is(err, core.ErrUnauthenticated):
		a.unauthenticated(w, r, "invalid API key")
		return core.Principal{}, false
	case err != nil:
		a.serverError(w, r, err)
		return core.Principal{}, false
	}
	return pr, true
}

func (a *API) unauthenticated(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="paymux"`)
	a.fail(w, r, http.StatusUnauthorized, "authentication_error", msg)
}

// allow checks authz for the key's own app and mode.
func (a *API) allow(w http.ResponseWriter, r *http.Request, pr core.Principal, act authz.Action) bool {
	actor := authz.Actor{Kind: authz.KindAPIKey, ID: pr.KeyID, OrgID: pr.OrgID, AppID: pr.AppID, Mode: pr.Mode}
	if authz.Can(actor, act, authz.Resource{OrgID: pr.OrgID, AppID: pr.AppID, Mode: pr.Mode}) {
		return true
	}
	a.fail(w, r, http.StatusForbidden, "permission_error", "this API key may not perform "+string(act))
	return false
}

func auditActor(pr core.Principal) audit.Actor {
	return audit.Actor{Type: audit.ActorAPIKey, ID: pr.KeyID}
}

func scope(pr core.Principal) core.Scope { return core.Scope{AppID: pr.AppID, Mode: pr.Mode} }

type errorBody struct {
	Error struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, status int, typ, msg string) {
	var b errorBody
	b.Error.Type, b.Error.Message, b.Error.RequestID = typ, msg, server.RequestID(r.Context())
	writeJSON(w, status, b)
}

// maxProviderMessage bounds the provider's message in an error response.
const maxProviderMessage = 300

// serverError answers an error that is none of the API's own. A provider's
// "this request is invalid or not allowed" (connector.ErrRejected) is the
// caller's to fix, so it is a 422 provider_rejected carrying the provider's
// message, passed through the log redactor (known secrets, key shapes, personal
// data) and cut to a bounded length; connector.ErrUnsupported is a 422
// unsupported. Everything else is an internal error that says nothing.
func (a *API) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("api request failed", "request_id", server.RequestID(r.Context()), "path", r.URL.Path, "err", err)
	switch {
	case errors.Is(err, connector.ErrRejected):
		a.fail(w, r, http.StatusUnprocessableEntity, "provider_rejected", a.providerMessage(err))
	case errors.Is(err, connector.ErrUnsupported):
		a.fail(w, r, http.StatusUnprocessableEntity, "unsupported", a.providerMessage(err))
	default:
		a.fail(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
}

// providerMessage is the text of a connector error made safe to show the caller.
func (a *API) providerMessage(err error) string {
	redactor := a.redact
	if redactor == nil {
		redactor = redact.New().Text
	}
	msg := strings.TrimSpace(redactor(err.Error()))
	if r := []rune(msg); len(r) > maxProviderMessage {
		msg = string(r[:maxProviderMessage]) + "..."
	}
	return msg
}

// failCore maps a domain error to its HTTP response.
func (a *API) failCore(w http.ResponseWriter, r *http.Request, err error) {
	var ce *core.Error
	if !errors.As(err, &ce) {
		a.serverError(w, r, err)
		return
	}
	status, typ := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, core.ErrInvalid):
		status, typ = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, core.ErrNotFound):
		status, typ = http.StatusNotFound, "not_found"
	case errors.Is(err, core.ErrInvalidState):
		status, typ = http.StatusConflict, "invalid_state"
	case errors.Is(err, core.ErrNotRefundable):
		status, typ = http.StatusConflict, "not_refundable"
	case errors.Is(err, core.ErrRefundExceeds):
		status, typ = http.StatusConflict, "refund_amount_exceeds_balance"
	case errors.Is(err, core.ErrNoRoute):
		status, typ = http.StatusUnprocessableEntity, "no_route"
	case errors.Is(err, core.ErrCurrencyNotAccepted):
		status, typ = http.StatusUnprocessableEntity, "currency_not_accepted"
	case errors.Is(err, core.ErrConflict):
		status, typ = http.StatusConflict, "conflict"
	}
	a.fail(w, r, status, typ, ce.Message)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody strictly decodes a JSON object body into dst. An empty body is
// allowed when allowEmpty is set (all fields optional).
func (a *API) decodeBody(w http.ResponseWriter, r *http.Request, body []byte, dst any, allowEmpty bool) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		if allowEmpty {
			return true
		}
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "a JSON request body is required")
		return false
	}
	if err := checkNoDuplicateKeys(body); err != nil {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", describeDecodeError(err))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", describeDecodeError(err))
		return false
	} else if dec.More() {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON: it has more than one value")
		return false
	}
	return true
}
