package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
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
	CheckoutURL string `json:"checkout_url"`
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
	res, err := a.core.CreatePayment(r.Context(), in)
	if err != nil {
		a.failCore(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, createdPaymentResp{
		PaymentView: core.ViewPayment(res.Payment),
		CheckoutURL: a.baseURL + "/pay/" + res.Payment.ID + "?t=" + url.QueryEscape(res.ClientSecret),
	})
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
		InitiatedBy: "api", PaymentID: r.PathValue("id"), AttemptID: req.AttemptID, Amount: req.Amount, Reason: req.Reason})
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
