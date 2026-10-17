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
	"net/http"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/store"
)

const maxBody = 1 << 20 // 1 MiB

// Options configures the API.
type Options struct {
	Core *core.Service
	DB   *store.DB
	// Box seals stored idempotent responses (they contain checkout secrets).
	Box *secrets.Box
	// BaseURL is PAYMUX_BASE_URL, the origin of checkout_url.
	BaseURL *url.URL
	Log     *slog.Logger
}

// Mux is the part of server.Server the API needs.
type Mux interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// API holds the handlers.
type API struct {
	core    *core.Service
	db      *store.DB
	box     *secrets.Box
	baseURL string
	log     *slog.Logger
}

// New builds the API.
func New(o Options) (*API, error) {
	if o.Core == nil || o.DB == nil || o.Box == nil || o.BaseURL == nil {
		return nil, errors.New("httpapi: Core, DB, Box and BaseURL are required")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &API{core: o.Core, db: o.DB, box: o.Box, baseURL: strings.TrimRight(o.BaseURL.String(), "/"), log: o.Log}, nil
}

// handler is an authenticated route handler. body is the already-read
// request body (nil for GET).
type handler func(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte)

// Register mounts the /v1 routes on m.
func (a *API) Register(m Mux) {
	m.HandleFunc("POST /v1/payments", a.wrap(a.createPayment, true))
	m.HandleFunc("GET /v1/payments/{id}", a.wrap(a.getPayment, false))
	m.HandleFunc("GET /v1/payments", a.wrap(a.listPayments, false))
	m.HandleFunc("POST /v1/payments/{id}/cancel", a.wrap(a.cancelPayment, true))
	m.HandleFunc("POST /v1/payments/{id}/refunds", a.wrap(a.createRefund, true))
	m.HandleFunc("GET /v1/refunds/{id}", a.wrap(a.getRefund, false))
	m.HandleFunc("/v1/", a.wrap(func(w http.ResponseWriter, r *http.Request, _ core.Principal, _ []byte) {
		a.fail(w, r, http.StatusNotFound, "not_found", "no such endpoint")
	}, false))
}

// wrap authenticates, reads the body of POSTs and applies idempotency.
func (a *API) wrap(h handler, idempotent bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pr, ok := a.authenticate(w, r)
		if !ok {
			return
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
		if idempotent && r.Header.Get("Idempotency-Key") != "" {
			a.idempotent(w, r, pr, body, h)
			return
		}
		h(w, r, pr, body)
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

func (a *API) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("api request failed", "request_id", server.RequestID(r.Context()), "path", r.URL.Path, "err", err)
	a.fail(w, r, http.StatusInternalServerError, "internal", "internal error")
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
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil || dec.More() {
		msg := "request body is not valid JSON for this endpoint"
		if err != nil {
			msg += ": " + err.Error()
		}
		a.fail(w, r, http.StatusBadRequest, "invalid_request", msg)
		return false
	}
	return true
}
