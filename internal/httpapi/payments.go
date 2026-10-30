package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

func (c customerReq) billingValues() url.Values {
	values := url.Values{"name": {c.Name}, "email": {c.Email}}
	if b := c.Billing; b != nil {
		values.Set("country", b.Country)
		values.Set("state_code", b.StateCode)
		values.Set("line1", b.Line1)
		values.Set("postal_code", b.PostalCode)
		values.Set("gstin", b.GSTIN)
		values.Set("legal_name", b.LegalName)
	}
	return values
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
	CheckoutMode string              `json:"checkout_mode"`
	CheckoutURL  string              `json:"checkout_url,omitempty"`
	Checkout     *checkoutActionResp `json:"checkout,omitempty"`
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
	if err := core.ValidateCreatePaymentInput(&in); err != nil {
		a.failCore(w, r, err)
		return
	}
	req.Currency = in.Currency
	mode, err := a.checkoutMode(r, pr.AppID)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, "internal", "checkout mode could not be loaded")
		return
	}
	if mode == "provider_action" {
		if a.checkout == nil || a.settings == nil {
			a.fail(w, r, http.StatusNotFound, "not_found", "provider-action checkout is unavailable")
			return
		}
		if r.Header.Get("Idempotency-Key") == "" {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "provider-action payment creation requires an Idempotency-Key")
			return
		}
		if failure := a.validateProviderActionBilling(r.Context(), pr.AppID, req); failure != nil {
			a.fail(w, r, failure.status, failure.typ, failure.message)
			return
		}
	}
	res, err := a.core.CreatePayment(r.Context(), in)
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	payment := res.Payment
	out := createdPaymentResp{PaymentView: core.ViewPayment(payment), CheckoutMode: mode}
	if mode == "hosted" {
		out.CheckoutURL = a.baseURL + "/pay/" + res.Payment.ID + "?t=" + url.QueryEscape(res.ClientSecret)
	} else {
		resolved, failure := a.resolveCheckoutAction(r.Context(), payment, checkoutActionReq{})
		if failure != nil {
			// A Paymux payment now exists. Retain this response under the request's
			// idempotency key so a retry cannot create a second payable payment.
			retainIdempotencyOnFailure(w)
			if failure.err != nil {
				a.log.Error("httpapi: provider-action checkout", "payment", payment.ID, "err", failure.err)
			}
			a.failPayment(w, r, failure.status, failure.typ, failure.message, payment.ID)
			return
		}
		payment = resolved.payment
		out.PaymentView = core.ViewPayment(payment)
		out.Checkout = &resolved.response
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

type checkoutActionFailure struct {
	status  int
	typ     string
	message string
	err     error
}

type resolvedCheckoutAction struct {
	response checkoutActionResp
	payment  store.Payment
}

func (a *API) validateProviderActionBilling(ctx context.Context, appID string, req createPaymentReq) *checkoutActionFailure {
	mode, issue, err := billingform.Settings(ctx, a.settings, appID)
	if err != nil {
		return &checkoutActionFailure{http.StatusInternalServerError, "internal", "billing policy could not be loaded", err}
	}
	var customer store.Customer
	hasCustomer := req.Customer != nil
	if c := req.Customer; c != nil {
		customer.Name, customer.Email = c.Name, c.Email
		if b := c.Billing; b != nil {
			customer.Billing = store.Billing{LegalName: b.LegalName, GSTIN: b.GSTIN, Line1: b.Line1, Line2: b.Line2,
				City: b.City, StateCode: b.StateCode, PostalCode: b.PostalCode, Country: b.Country}
		}
	}
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	need := billingform.NeedFor(mode, issue, currency, *req.Amount, customer, hasCustomer)
	if need.Show {
		return &checkoutActionFailure{status: http.StatusUnprocessableEntity, typ: "billing_required",
			message: "provider-action checkout requires customer billing details during payment creation"}
	}
	if req.Customer != nil && req.Customer.Billing != nil {
		if _, message := billingform.Parse(req.Customer.billingValues(), currency, need); message != "" {
			return &checkoutActionFailure{status: http.StatusUnprocessableEntity, typ: "invalid_billing",
				message: "billing details are incomplete or invalid (" + message + ")"}
		}
	}
	return nil
}

func (a *API) resolveCheckoutAction(ctx context.Context, p store.Payment, req checkoutActionReq) (*resolvedCheckoutAction, *checkoutActionFailure) {
	switch state.PaymentStatus(p.Status) {
	case state.PaymentSucceeded, state.PaymentCanceled, state.PaymentNeedsReview, state.PaymentExpired:
		return &resolvedCheckoutAction{response: checkoutActionResp{Kind: "complete", PaymentID: p.ID, Status: p.Status}, payment: p}, nil
	}
	if a.now().After(p.ExpiresAt) {
		return &resolvedCheckoutAction{response: checkoutActionResp{Kind: "complete", PaymentID: p.ID, Status: string(state.PaymentExpired)}, payment: p}, nil
	}
	switch state.PaymentStatus(p.Status) {
	case state.PaymentCreated, state.PaymentRequiresAction, state.PaymentFailed:
	default:
		return nil, &checkoutActionFailure{status: http.StatusConflict, typ: "state_conflict", message: "payment cannot be started in its current state"}
	}
	var customer store.Customer
	var err error
	if p.CustomerID != "" {
		customer, err = a.core.GetCustomer(ctx, p.CustomerID)
		if err != nil {
			return nil, &checkoutActionFailure{http.StatusInternalServerError, "internal", "customer details could not be loaded", err}
		}
	}
	billingMode, issueInvoices, err := billingform.Settings(ctx, a.settings, p.AppID)
	if err != nil {
		return nil, &checkoutActionFailure{http.StatusInternalServerError, "internal", "billing policy could not be loaded", err}
	}
	need := billingform.NeedFor(billingMode, issueInvoices, p.Currency, p.Amount, customer, p.CustomerID != "")
	if req.Billing != nil {
		in, message := billingform.Parse(req.Billing.values(), p.Currency, need)
		if message != "" {
			return nil, &checkoutActionFailure{status: http.StatusUnprocessableEntity, typ: "invalid_billing",
				message: "billing details are incomplete or invalid (" + message + ")"}
		}
		if err := a.core.SetBilling(ctx, p.ID, in); err != nil {
			return nil, &checkoutActionFailure{http.StatusInternalServerError, "internal", "billing details could not be saved", err}
		}
		need.Show = false
	}
	if need.Show {
		return nil, &checkoutActionFailure{status: http.StatusUnprocessableEntity, typ: "billing_required",
			message: "provider-action checkout requires the merchant backend to supply the required billing details"}
	}
	start := a.checkout.StartCheckout
	if req.Fresh {
		start = a.checkout.StartFreshCheckout
	}
	started, err := start(ctx, p.ID)
	if err != nil {
		switch {
		case errors.Is(err, checkout.ErrCheckoutFailed):
			return nil, &checkoutActionFailure{http.StatusBadGateway, "provider_unavailable", "no payment provider is available; try checkout-action", err}
		case errors.Is(err, core.ErrNoRoute), errors.Is(err, core.ErrCurrencyNotAccepted):
			return nil, &checkoutActionFailure{http.StatusUnprocessableEntity, "no_route", "no payment method is available", err}
		case errors.Is(err, core.ErrInvalidState):
			return nil, &checkoutActionFailure{http.StatusConflict, "state_conflict", "payment cannot be started in its current state", err}
		default:
			return nil, &checkoutActionFailure{http.StatusInternalServerError, "internal", "checkout could not be started", err}
		}
	}
	latest, err := a.core.GetPayment(ctx, core.Scope{AppID: p.AppID, Mode: p.Mode}, p.ID)
	if err != nil {
		return nil, &checkoutActionFailure{http.StatusInternalServerError, "internal", "payment status could not be loaded", err}
	}
	action := checkout.MerchantBrowserAction(started.Action)
	return &resolvedCheckoutAction{payment: latest, response: checkoutActionResp{Kind: "checkout", PaymentID: p.ID,
		Status: latest.Status, Launcher: started.Connector, Action: &action}}, nil
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
	var req checkoutActionReq
	if !a.decodeBody(w, r, body, &req, true) {
		return
	}
	resolved, failure := a.resolveCheckoutAction(r.Context(), p, req)
	if failure != nil {
		if failure.err != nil && failure.status == http.StatusInternalServerError {
			a.log.Error("httpapi: provider-action checkout", "payment", p.ID, "err", failure.err)
		}
		a.fail(w, r, failure.status, failure.typ, failure.message)
		return
	}
	writeJSON(w, http.StatusOK, resolved.response)
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
