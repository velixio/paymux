package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/velixio/paymux/internal/authz"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/payouts"
	"github.com/velixio/paymux/pkg/connector"
)

func (a *API) payoutsEnabled(w http.ResponseWriter, r *http.Request) bool {
	if a.payouts == nil {
		a.fail(w, r, http.StatusNotFound, "not_found", "payouts are not enabled")
		return false
	}
	return true
}

func (a *API) failPayout(w http.ResponseWriter, r *http.Request, err error) {
	var pe *payouts.Error
	if !errors.As(err, &pe) {
		a.serverError(w, r, err)
		return
	}
	status, typ := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, payouts.ErrInvalid):
		status, typ = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, payouts.ErrNotFound):
		status, typ = http.StatusNotFound, "not_found"
	case errors.Is(err, payouts.ErrNoRoute):
		status, typ = http.StatusUnprocessableEntity, "no_route"
	case errors.Is(err, payouts.ErrNeedsReview):
		status, typ = http.StatusConflict, "payout_needs_review"
	case errors.Is(err, payouts.ErrKeyReuse):
		status, typ = http.StatusConflict, "idempotency_key_reuse"
	}
	a.fail(w, r, status, typ, pe.Message)
}

type createPayoutReq struct {
	Amount            *int64 `json:"amount"`
	Currency          string `json:"currency"`
	ProviderAccountID string `json:"provider_account_id"`
	Beneficiary       *struct {
		Name          string `json:"name"`
		AccountNumber string `json:"account_number"`
		IFSC          string `json:"ifsc"`
		UPI           string `json:"upi"`
		Email         string `json:"email"`
		Phone         string `json:"phone"`
	} `json:"beneficiary"`
	Narration string            `json:"narration"`
	Metadata  map[string]string `json:"metadata"`
	// Force sends the payout although one to the same beneficiary and amount
	// is in needs_review. It is audited.
	Force bool `json:"force"`
}

// createPayout sends money out. It needs the payout.create action and the
// explicit payouts:write scope: a key without a scope list cannot do it.
func (a *API) createPayout(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.payoutsEnabled(w, r) || !a.allow(w, r, pr, authz.PayoutCreate) {
		return
	}
	var req createPayoutReq
	if !a.decodeBody(w, r, body, &req, false) {
		return
	}
	if req.Amount == nil || req.Beneficiary == nil {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "amount (minor units) and beneficiary are required")
		return
	}
	b := req.Beneficiary
	p, err := a.payouts.Create(r.Context(), payouts.CreateInput{Scope: scope(pr), Amount: *req.Amount, Currency: req.Currency,
		Beneficiary: connector.Beneficiary{Name: b.Name, AccountNumber: b.AccountNumber, IFSC: b.IFSC, UPI: b.UPI, Email: b.Email, Phone: b.Phone},
		Narration:   req.Narration, Metadata: req.Metadata, ProviderAccountID: req.ProviderAccountID,
		Force: req.Force, Actor: auditActor(pr), IdempotencyKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		a.failPayout(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, payouts.ViewPayout(p))
}

func (a *API) getPayout(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.payoutsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	p, err := a.payouts.Get(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failPayout(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, payouts.ViewPayout(p))
}

func (a *API) listPayouts(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.payoutsEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	q := r.URL.Query()
	limit := 10
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 100")
			return
		}
		limit = n
	}
	rows, err := a.payouts.List(r.Context(), scope(pr), q.Get("status"), q.Get("starting_after"), limit+1)
	if err != nil {
		a.failPayout(w, r, err)
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	data := make([]payouts.View, 0, len(rows))
	for _, p := range rows {
		data = append(data, payouts.ViewPayout(p))
	}
	resp := map[string]any{"object": "list", "data": data, "has_more": more}
	if more {
		resp["next_cursor"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}
