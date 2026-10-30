package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/billingform"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

type customerReq struct {
	ExternalRef string      `json:"external_ref"`
	Name        string      `json:"name"`
	Email       string      `json:"email"`
	Phone       string      `json:"phone"`
	Billing     *billingReq `json:"billing"`
}

type billingReq struct {
	LegalName  string `json:"legal_name"`
	GSTIN      string `json:"gstin"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	City       string `json:"city"`
	StateCode  string `json:"state_code"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

type createPaymentReq struct {
	Amount            *int64            `json:"amount"`
	Currency          string            `json:"currency"`
	OrderRef          string            `json:"order_ref"`
	Description       string            `json:"description"`
	Customer          *customerReq      `json:"customer"`
	Metadata          map[string]string `json:"metadata"`
	ReturnURL         string            `json:"return_url"`
	AllowedConnectors []string          `json:"allowed_connectors"`
	Country           string            `json:"country"`
}

type createdPaymentResp struct {
	core.PaymentView
	CheckoutMode string `json:"checkout_mode"`
	CheckoutURL  string `json:"checkout_url,omitempty"`
}

// checkoutMode resolves the App's response mode, preserving hosted checkout
// for API embedders that do not install the settings service.
func (a *API) checkoutMode(r *http.Request, appID string) (string, error) {
	if a.settings == nil {
		return "hosted", nil
	}
	v, err := a.settings.Resolve(r.Context(), appID, "checkout_mode")
	if err != nil {
		return "", err
	}
	var mode string
	if err := v.Decode(&mode); err != nil {
		return "", err
	}
	return mode, nil
}

func (a *API) createPayment(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	var req createPaymentReq
	if !a.decodeBody(w, r, body, &req, false) {
		return
	}
	if req.Amount == nil {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "amount is required (integer, minor units)")
		return
	}
	mode, err := a.checkoutMode(r, pr.AppID)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "internal", "checkout mode could not be loaded")
		return
	}
	in := core.CreatePaymentInput{Scope: scope(pr), Amount: *req.Amount, Currency: req.Currency, OrderRef: req.OrderRef,
		Description: req.Description, Metadata: req.Metadata, ReturnURL: req.ReturnURL,
		Country: req.Country, AllowedConnectors: req.AllowedConnectors}
	if c := req.Customer; c != nil {
		in.Customer = &core.CustomerInput{ExternalRef: c.ExternalRef, Name: c.Name, Email: c.Email, Phone: c.Phone}
		if b := c.Billing; b != nil {
			in.Customer.Billing = store.Billing{LegalName: b.LegalName, GSTIN: b.GSTIN, Line1: b.Line1, Line2: b.Line2,
				City: b.City, StateCode: b.StateCode, PostalCode: b.PostalCode, Country: b.Country}
		}
	}
	res, err := a.core.CreatePayment(r.Context(), in)
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	out := createdPaymentResp{PaymentView: core.ViewPayment(res.Payment), CheckoutMode: mode}
	if mode == "hosted" {
		out.CheckoutURL = a.baseURL + "/pay/" + res.Payment.ID + "?t=" + url.QueryEscape(res.ClientSecret)
	}
	writeJSON(w, http.StatusCreated, out)
}

type checkoutActionReq struct {
	Fresh   bool                   `json:"fresh"`
	Billing *checkoutActionBilling `json:"billing,omitempty"`
}

type checkoutActionBilling struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Country    string `json:"country"`
	StateCode  string `json:"state_code"`
	Line1      string `json:"line1"`
	PostalCode string `json:"postal_code"`
	GSTIN      string `json:"gstin"`
	LegalName  string `json:"legal_name"`
}

func (b checkoutActionBilling) values() url.Values {
	return url.Values{"name": {b.Name}, "email": {b.Email}, "country": {b.Country}, "state_code": {b.StateCode},
		"line1": {b.Line1}, "postal_code": {b.PostalCode}, "gstin": {b.GSTIN}, "legal_name": {b.LegalName}}
}

type checkoutActionResp struct {
	Kind      string                `json:"kind"`
	PaymentID string                `json:"payment_id"`
	Status    string                `json:"status"`
	Launcher  string                `json:"launcher,omitempty"`
	Action    *connector.NextAction `json:"action,omitempty"`
}

// createCheckoutAction is called by the merchant backend. It returns only
// public provider-launch data, so the checkout URL and customer token never
// need to enter the merchant's browser.
func (a *API) createCheckoutAction(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if a.checkout == nil || a.settings == nil {
		a.fail(w, r, http.StatusNotFound, "not_found", "provider-action checkout is unavailable")
		return
	}
	p, err := a.core.GetPayment(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	switch state.PaymentStatus(p.Status) {
	case state.PaymentSucceeded, state.PaymentCanceled, state.PaymentNeedsReview, state.PaymentExpired:
		writeJSON(w, http.StatusOK, checkoutActionResp{Kind: "complete", PaymentID: p.ID, Status: p.Status})
		return
	}
	if a.now().After(p.ExpiresAt) {
		writeJSON(w, http.StatusOK, checkoutActionResp{Kind: "complete", PaymentID: p.ID, Status: string(state.PaymentExpired)})
		return
	}
	switch state.PaymentStatus(p.Status) {
	case state.PaymentCreated, state.PaymentRequiresAction, state.PaymentFailed:
		// These states can safely start or retry customer checkout.
	default:
		a.fail(w, r, http.StatusConflict, "state_conflict", "payment cannot be started in its current state")
		return
	}
	var req checkoutActionReq
	if !a.decodeBody(w, r, body, &req, true) {
		return
	}
	var customer store.Customer
	if p.CustomerID != "" {
		customer, err = a.core.GetCustomer(r.Context(), p.CustomerID)
		if err != nil {
			a.fail(w, r, http.StatusInternalServerError, "internal", "customer details could not be loaded")
			return
		}
	}
	billingMode, issueInvoices, err := billingform.Settings(r.Context(), a.settings, p.AppID)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "internal", "billing policy could not be loaded")
		return
	}
	need := billingform.NeedFor(billingMode, issueInvoices, p.Currency, p.Amount, customer, p.CustomerID != "")
	if req.Billing != nil {
		in, message := billingform.Parse(req.Billing.values(), p.Currency, need)
		if message != "" {
			a.fail(w, r, http.StatusUnprocessableEntity, "invalid_billing", "billing details are incomplete or invalid ("+message+")")
			return
		}
		if err := a.core.SetBilling(r.Context(), p.ID, in); err != nil {
			a.fail(w, r, http.StatusInternalServerError, "internal", "billing details could not be saved")
			return
		}
		need.Show = false
	}
	if need.Show {
		a.fail(w, r, http.StatusUnprocessableEntity, "billing_required", "provider-action checkout requires the merchant backend to supply the required billing details")
		return
	}
	start := a.checkout.StartCheckout
	if req.Fresh {
		start = a.checkout.StartFreshCheckout
	}
	started, err := start(r.Context(), p.ID)
	switch {
	case err == nil:
		action := started.Action
		writeJSON(w, http.StatusOK, checkoutActionResp{Kind: "checkout", PaymentID: p.ID, Status: p.Status,
			Launcher: started.Connector, Action: &action})
	case errors.Is(err, checkout.ErrCheckoutFailed):
		a.fail(w, r, http.StatusBadGateway, "provider_unavailable", "no payment provider is available; try again")
	case errors.Is(err, core.ErrNoRoute), errors.Is(err, core.ErrCurrencyNotAccepted):
		a.fail(w, r, http.StatusUnprocessableEntity, "no_route", "no payment method is available")
	case errors.Is(err, core.ErrInvalidState):
		a.fail(w, r, http.StatusConflict, "state_conflict", "payment cannot be started in its current state")
	default:
		a.log.Error("httpapi: provider-action checkout", "payment", p.ID, "err", err)
		a.fail(w, r, http.StatusInternalServerError, "internal", "checkout could not be started")
	}
}

func (a *API) getPayment(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	p, err := a.core.GetPayment(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	attempts, err := a.core.PaymentAttempts(r.Context(), scope(pr), p.ID)
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paymentDetail{PaymentView: core.ViewPayment(p), Attempts: attempts})
}

// paymentDetail is GET /v1/payments/{id}: the payment and its attempts.
type paymentDetail struct {
	core.PaymentView
	Attempts []core.AttemptView `json:"attempts"`
}

type listResp struct {
	Object     string             `json:"object"`
	Data       []core.PaymentView `json:"data"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func (a *API) listPayments(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	q := r.URL.Query()
	lp := core.ListParams{Status: q.Get("status"), After: q.Get("starting_after")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 100")
			return
		}
		lp.Limit = n
	}
	page, err := a.core.ListPayments(r.Context(), scope(pr), lp)
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	out := listResp{Object: "list", Data: make([]core.PaymentView, 0, len(page.Payments)), HasMore: page.HasMore, NextCursor: page.NextCursor}
	for _, p := range page.Payments {
		out.Data = append(out.Data, core.ViewPayment(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) cancelPayment(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.allow(w, r, pr, authz.PaymentCancel) {
		return
	}
	p, err := a.core.CancelPayment(r.Context(), scope(pr), auditActor(pr), r.PathValue("id"))
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, core.ViewPayment(p))
}

type createRefundReq struct {
	Amount    int64  `json:"amount"`
	AttemptID string `json:"attempt_id"`
	Reason    string `json:"reason"`
}

func (a *API) createRefund(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.allow(w, r, pr, authz.RefundCreate) {
		return
	}
	var req createRefundReq
	if !a.decodeBody(w, r, body, &req, true) {
		return
	}
	ref, err := a.core.CreateRefund(r.Context(), core.CreateRefundInput{Scope: scope(pr), Actor: auditActor(pr),
		InitiatedBy: "api", PaymentID: r.PathValue("id"), AttemptID: req.AttemptID, Amount: req.Amount, Reason: req.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, core.ViewRefund(ref))
}

func (a *API) getRefund(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.allow(w, r, pr, authz.RefundRead) {
		return
	}
	ref, err := a.core.GetRefund(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, core.ViewRefund(ref))
}
