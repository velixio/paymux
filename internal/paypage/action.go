package paypage

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/velixio/paymux/internal/billingform"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
	"github.com/velixio/paymux/pkg/connector"
)

const checkoutTokenHeader = "Paymux-Checkout-Token"

type actionRequest struct {
	Fresh   bool           `json:"fresh"`
	Billing *actionBilling `json:"billing,omitempty"`
}

type actionBilling struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Country    string `json:"country"`
	StateCode  string `json:"state_code"`
	Line1      string `json:"line1"`
	PostalCode string `json:"postal_code"`
	GSTIN      string `json:"gstin"`
	LegalName  string `json:"legal_name"`
}

type actionState struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type billingAction struct {
	Values          actionBilling `json:"values"`
	States          []actionState `json:"states,omitempty"`
	StateRequired   bool          `json:"state_required"`
	AddressRequired bool          `json:"address_required"`
	CountryRequired bool          `json:"country_required"`
}

type actionResponse struct {
	Kind      string                `json:"kind"`
	PaymentID string                `json:"payment_id"`
	Status    string                `json:"status"`
	Launcher  string                `json:"launcher,omitempty"`
	Action    *connector.NextAction `json:"action,omitempty"`
	Billing   *billingAction        `json:"billing,omitempty"`
}

type actionError struct {
	Error struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

func actionJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func actionFail(w http.ResponseWriter, status int, typ, message string) {
	var out actionError
	out.Error.Type, out.Error.Message, out.Error.RequestID = typ, message, w.Header().Get("X-Request-Id")
	actionJSON(w, status, out)
}

func paymentOrigin(p store.Payment) string {
	u, err := url.Parse(p.ReturnURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func setActionCORS(w http.ResponseWriter, r *http.Request, p store.Payment) bool {
	origin := strings.ToLower(strings.TrimSpace(r.Header.Get("Origin")))
	if origin == "" || origin == "null" || origin != paymentOrigin(p) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, "+checkoutTokenHeader)
	h.Set("Access-Control-Max-Age", "600")
	h.Add("Vary", "Origin")
	return true
}

func (h *Handler) actionPayment(w http.ResponseWriter, r *http.Request) (store.Payment, bool) {
	p, err := h.o.Core.GetPaymentByID(r.Context(), r.PathValue("id"))
	if err != nil || !h.hostMayServe(r.Context(), p.AppID) {
		actionFail(w, http.StatusNotFound, "not_found", "checkout session not found")
		return store.Payment{}, false
	}
	if !setActionCORS(w, r, p) {
		actionFail(w, http.StatusForbidden, "origin_not_allowed", "request origin must match the payment return URL")
		return store.Payment{}, false
	}
	return p, true
}

func (h *Handler) actionOptions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actionPayment(w, r); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) action(w http.ResponseWriter, r *http.Request) {
	p, ok := h.actionPayment(w, r)
	if !ok {
		return
	}
	secret := r.Header.Get(checkoutTokenHeader)
	if secret == "" || !core.ClientSecretMatches(p, secret) {
		actionFail(w, http.StatusNotFound, "not_found", "checkout session not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req actionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		actionFail(w, http.StatusBadRequest, "invalid_request", "request body is not valid JSON")
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		actionFail(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	if h.actionComplete(w, p) {
		return
	}
	customer, need, err := h.actionBillingNeed(r, p)
	if err != nil {
		h.o.Logger.Error("paypage: browser checkout billing", "payment", p.ID, "err", err)
		actionFail(w, http.StatusInternalServerError, "internal", "checkout could not be prepared")
		return
	}
	if need.Show {
		if req.Billing == nil {
			actionJSON(w, http.StatusOK, actionResponse{Kind: "billing", PaymentID: p.ID, Status: p.Status, Billing: browserBilling(customer, p, need)})
			return
		}
		in, msg := billingform.Parse(req.Billing.values(), p.Currency, need)
		if msg != "" {
			actionFail(w, http.StatusUnprocessableEntity, msg, billingMessage(msg))
			return
		}
		if err := h.o.Core.SetBilling(r.Context(), p.ID, in); err != nil {
			h.o.Logger.Error("paypage: browser checkout set billing", "payment", p.ID, "err", err)
			actionFail(w, http.StatusInternalServerError, "internal", "billing details could not be saved")
			return
		}
	}
	start := h.o.Flow.StartCheckout
	if req.Fresh {
		start = h.o.Flow.StartFreshCheckout
	}
	started, err := start(r.Context(), p.ID)
	switch {
	case err == nil:
		a := merchantBrowserAction(started.Action)
		actionJSON(w, http.StatusOK, actionResponse{Kind: "checkout", PaymentID: p.ID, Status: p.Status, Launcher: started.Connector, Action: &a})
	case errors.Is(err, checkout.ErrCheckoutFailed):
		actionFail(w, http.StatusBadGateway, "provider_unavailable", "no payment provider is available; try again")
	case errors.Is(err, core.ErrNoRoute), errors.Is(err, core.ErrCurrencyNotAccepted):
		actionFail(w, http.StatusUnprocessableEntity, "no_route", "no payment method is available")
	case errors.Is(err, core.ErrInvalidState):
		fresh, getErr := h.o.Core.GetPaymentByID(r.Context(), p.ID)
		if getErr == nil && h.actionComplete(w, fresh) {
			return
		}
		actionFail(w, http.StatusConflict, "state_conflict", "payment cannot be started in its current state")
	default:
		h.o.Logger.Error("paypage: browser checkout", "payment", p.ID, "err", err)
		actionFail(w, http.StatusInternalServerError, "internal", "checkout could not be started")
	}
}

// merchantBrowserAction removes a provider callback to Paymux from an SDK
// action returned to merchant JavaScript. The merchant-side SDK completes via
// the provider's handler while webhooks and provider fetches remain
// authoritative. Hosted Paymux pages do not use this response and retain their
// callback flow.
func merchantBrowserAction(action connector.NextAction) connector.NextAction {
	if action.Kind != connector.ActionSDK || action.Options == nil {
		return action
	}
	if _, hasCallback := action.Options["callback_url"]; !hasCallback {
		return action
	}
	options := make(map[string]any, len(action.Options)-1)
	for key, value := range action.Options {
		if key != "callback_url" && key != "redirect" {
			options[key] = value
		}
	}
	action.Options = options
	return action
}

func (h *Handler) actionComplete(w http.ResponseWriter, p store.Payment) bool {
	finished := false
	switch state.PaymentStatus(p.Status) {
	case state.PaymentSucceeded, state.PaymentCanceled, state.PaymentNeedsReview, state.PaymentExpired:
		finished = true
	}
	if !finished && h.o.Now().After(p.ExpiresAt) {
		p.Status, finished = string(state.PaymentExpired), true
	}
	if finished {
		actionJSON(w, http.StatusOK, actionResponse{Kind: "complete", PaymentID: p.ID, Status: p.Status})
	}
	return finished
}

func (h *Handler) actionBillingNeed(r *http.Request, p store.Payment) (store.Customer, billingform.Need, error) {
	var customer store.Customer
	if p.CustomerID != "" {
		var err error
		if customer, err = h.o.Core.GetCustomer(r.Context(), p.CustomerID); err != nil {
			return customer, billingform.Need{}, err
		}
	}
	need, err := h.billingNeed(r.Context(), p, customer)
	return customer, need, err
}

func browserBilling(customer store.Customer, p store.Payment, need billingform.Need) *billingAction {
	b := customer.Billing
	values := actionBilling{Name: customer.Name, Email: customer.Email, Country: b.Country, StateCode: b.StateCode,
		Line1: b.Line1, PostalCode: b.PostalCode, GSTIN: b.GSTIN, LegalName: b.LegalName}
	if values.Country == "" && p.Currency == "INR" {
		values.Country = "IN"
	}
	out := &billingAction{Values: values, StateRequired: need.StateRequired, AddressRequired: need.AddressRequired, CountryRequired: need.CountryRequired}
	if p.Currency == "INR" || strings.EqualFold(b.Country, "IN") {
		codes := tax.StateCodes()
		keys := make([]string, 0, len(codes))
		for code := range codes {
			keys = append(keys, code)
		}
		sort.Strings(keys)
		for _, code := range keys {
			out.States = append(out.States, actionState{Code: code, Name: codes[code]})
		}
	}
	return out
}

func (b actionBilling) values() url.Values {
	return url.Values{"name": {b.Name}, "email": {b.Email}, "country": {b.Country}, "state_code": {b.StateCode},
		"line1": {b.Line1}, "postal_code": {b.PostalCode}, "gstin": {b.GSTIN}, "legal_name": {b.LegalName}}
}

func billingMessage(code string) string {
	messages := map[string]string{
		"billing.err.too_long": "One or more billing fields are too long.", "billing.err.name": "Enter the customer name.",
		"billing.err.email": "Enter a valid email address.", "billing.err.country": "Enter a two-letter country code.",
		"billing.err.country_required": "Enter the customer country.", "billing.err.state_invalid": "Select a valid Indian state.",
		"billing.err.state_required": "Select the customer state.", "billing.err.gstin": "Enter a valid GSTIN.",
		"billing.err.legal_name": "Enter the legal name for this GSTIN.", "billing.err.gst_state": "GSTIN and selected state do not match.",
		"billing.err.address": "Enter the billing address.", "billing.err.pin": "Enter a valid six-digit PIN code.",
	}
	if message := messages[code]; message != "" {
		return message
	}
	return "Check the billing details and try again."
}
