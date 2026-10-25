package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/subscriptions"
)

func (a *API) subsEnabled(w http.ResponseWriter, r *http.Request) bool {
	if a.subs == nil {
		a.fail(w, r, http.StatusNotFound, "not_found", "subscriptions are not enabled")
		return false
	}
	return true
}

// failSub maps a subscriptions error to its HTTP response.
func (a *API) failSub(w http.ResponseWriter, r *http.Request, err error) {
	var se *subscriptions.Error
	if !errors.As(err, &se) {
		a.serverError(w, r, err)
		return
	}
	status, typ := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, subscriptions.ErrInvalid):
		status, typ = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, subscriptions.ErrNotFound):
		status, typ = http.StatusNotFound, "not_found"
	case errors.Is(err, subscriptions.ErrInvalidState):
		status, typ = http.StatusConflict, "invalid_state"
	case errors.Is(err, subscriptions.ErrAmountExceedsMandateLimit):
		status, typ = http.StatusUnprocessableEntity, "amount_exceeds_mandate_limit"
	case errors.Is(err, subscriptions.ErrNoRoute):
		status, typ = http.StatusUnprocessableEntity, "no_route"
	case errors.Is(err, subscriptions.ErrUnsupported):
		status, typ = http.StatusUnprocessableEntity, "unsupported"
	}
	a.fail(w, r, status, typ, se.Message)
}

type createSubscriptionReq struct {
	Customer      *customerReq      `json:"customer"`
	Amount        *int64            `json:"amount"`
	Currency      string            `json:"currency"`
	Interval      string            `json:"interval"`
	IntervalCount int               `json:"interval_count"`
	TrialDays     int               `json:"trial_days"`
	StartAt       *time.Time        `json:"start_at"`
	TotalCount    int               `json:"total_count"`
	Description   string            `json:"description"`
	Metadata      map[string]string `json:"metadata"`
	ReturnURL     string            `json:"return_url"`
	Method        string            `json:"method"`
	// AllowedConnectors and Country restrict and steer routing, as for a payment.
	AllowedConnectors []string `json:"allowed_connectors"`
	Country           string   `json:"country"`
}

// subscriptionResp is a subscription, plus the checkout link where one was issued.
type subscriptionResp struct {
	subscriptions.View
	CheckoutURL string `json:"checkout_url,omitempty"`
	// Proration says what a prorated plan change did (update only).
	Proration *subscriptions.ProrationOutcome `json:"proration,omitempty"`
}

func (a *API) createSubscription(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.subsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	var req createSubscriptionReq
	if !a.decodeBody(w, r, body, &req, false) {
		return
	}
	if req.Amount == nil {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "amount is required (integer, minor units per period)")
		return
	}
	in := subscriptions.CreateInput{Scope: scope(pr), Amount: *req.Amount, Currency: req.Currency, Interval: req.Interval,
		IntervalCount: req.IntervalCount, TrialDays: req.TrialDays, TotalCount: req.TotalCount, Description: req.Description,
		Metadata: req.Metadata, ReturnURL: req.ReturnURL, Method: req.Method,
		AllowedConnectors: req.AllowedConnectors, Country: req.Country}
	if req.StartAt != nil {
		in.StartAt = *req.StartAt
	}
	if c := req.Customer; c != nil {
		in.Customer = &core.CustomerInput{ExternalRef: c.ExternalRef, Name: c.Name, Email: c.Email, Phone: c.Phone}
		if b := c.Billing; b != nil {
			in.Customer.Billing = store.Billing{LegalName: b.LegalName, GSTIN: b.GSTIN, Line1: b.Line1, Line2: b.Line2,
				City: b.City, StateCode: b.StateCode, PostalCode: b.PostalCode, Country: b.Country}
		}
	}
	res, err := a.subs.Create(r.Context(), in)
	if err != nil {
		a.failSub(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, subscriptionResp{subscriptions.ViewSubscription(res.Subscription), res.CheckoutURL, nil})
}

func (a *API) getSubscription(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.subsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	sub, err := a.subs.Get(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failSub(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, subscriptions.ViewSubscription(sub))
}

func (a *API) listSubscriptions(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.subsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	q := r.URL.Query()
	lp := subscriptions.ListParams{CustomerID: q.Get("customer"), Status: q.Get("status"), StartingAfter: q.Get("starting_after")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 100")
			return
		}
		lp.Limit = n
	}
	page, err := a.subs.List(r.Context(), scope(pr), lp)
	if err != nil {
		a.failSub(w, r, err)
		return
	}
	data := make([]subscriptions.View, 0, len(page.Data))
	for _, s := range page.Data {
		data = append(data, subscriptions.ViewSubscription(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": page.HasMore, "next_cursor": page.NextCursor})
}

func (a *API) cancelSubscription(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.subsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentCancel) {
		return
	}
	var req struct {
		AtPeriodEnd bool `json:"at_period_end"`
	}
	if !a.decodeBody(w, r, body, &req, true) {
		return
	}
	sub, err := a.subs.CancelBy(r.Context(), scope(pr), r.PathValue("id"), req.AtPeriodEnd, auditActor(pr))
	if err != nil {
		a.failSub(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, subscriptions.ViewSubscription(sub))
}

func (a *API) updateSubscription(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.subsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	var req struct {
		Amount    *int64 `json:"amount"`
		Proration string `json:"proration"`
	}
	if !a.decodeBody(w, r, body, &req, false) {
		return
	}
	if req.Amount == nil {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "amount is required (integer, minor units per period)")
		return
	}
	res, err := a.subs.Update(r.Context(), scope(pr), r.PathValue("id"),
		subscriptions.UpdateInput{Amount: *req.Amount, Proration: subscriptions.Proration(req.Proration)})
	if err != nil {
		a.failSub(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, subscriptionResp{subscriptions.ViewSubscription(res.Subscription), res.CheckoutURL, res.Proration})
}

func (a *API) subscriptionPaymentMethod(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.subsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	var req struct {
		Method string `json:"method"`
	}
	if !a.decodeBody(w, r, body, &req, true) {
		return
	}
	res, err := a.subs.UpdatePaymentMethod(r.Context(), scope(pr), r.PathValue("id"), req.Method)
	if err != nil {
		a.failSub(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, subscriptionResp{subscriptions.ViewSubscription(res.Subscription), res.CheckoutURL, nil})
}
