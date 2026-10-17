package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/paylinks"
)

func (a *API) linksEnabled(w http.ResponseWriter, r *http.Request) bool {
	if a.links == nil {
		a.fail(w, r, http.StatusNotFound, "not_found", "payment links are not enabled")
		return false
	}
	return true
}

// failLink maps a paylinks error to its HTTP response.
func (a *API) failLink(w http.ResponseWriter, r *http.Request, err error) {
	var le *paylinks.Error
	if !errors.As(err, &le) {
		a.serverError(w, r, err)
		return
	}
	status, typ := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, paylinks.ErrInvalid):
		status, typ = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, paylinks.ErrNotFound):
		status, typ = http.StatusNotFound, "not_found"
	}
	a.fail(w, r, status, typ, le.Message)
}

type createLinkReq struct {
	Amount      *int64            `json:"amount"`
	MinAmount   int64             `json:"min_amount"`
	MaxAmount   int64             `json:"max_amount"`
	Currency    string            `json:"currency"`
	Description string            `json:"description"`
	ReturnURL   string            `json:"return_url"`
	SingleUse   bool              `json:"single_use"`
	ExpiresAt   *time.Time        `json:"expires_at"`
	Metadata    map[string]string `json:"metadata"`
}

func (a *API) createPaymentLink(w http.ResponseWriter, r *http.Request, pr core.Principal, body []byte) {
	if !a.linksEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	var req createLinkReq
	if !a.decodeBody(w, r, body, &req, false) {
		return
	}
	in := paylinks.CreateInput{Scope: scope(pr), MinAmount: req.MinAmount, MaxAmount: req.MaxAmount, Currency: req.Currency,
		Description: req.Description, ReturnURL: req.ReturnURL, SingleUse: req.SingleUse, Metadata: req.Metadata}
	if req.Amount != nil {
		in.Amount = *req.Amount
	}
	if req.ExpiresAt != nil {
		in.ExpiresAt = *req.ExpiresAt
	}
	l, err := a.links.Create(r.Context(), auditActor(pr), in)
	if err != nil {
		a.failLink(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, paylinks.ViewLink(l, a.links.URL(l)))
}

func (a *API) getPaymentLink(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.linksEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	l, err := a.links.Get(r.Context(), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failLink(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paylinks.ViewLink(l, a.links.URL(l)))
}

func (a *API) listPaymentLinks(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.linksEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	q := r.URL.Query()
	limit := 25
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be an integer from 1 to 100")
			return
		}
		limit = n
	}
	p := paylinks.ListParams{StartingAfter: q.Get("starting_after"), Limit: limit + 1} // one more, to know whether there is a next page
	if v := q.Get("active"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil || v != "true" && v != "false" {
			a.fail(w, r, http.StatusBadRequest, "invalid_request", "active must be true or false")
			return
		}
		p.Active = &on
	}
	page, err := a.links.List(r.Context(), scope(pr), p)
	if err != nil {
		a.failLink(w, r, err)
		return
	}
	hasMore := len(page) > limit
	if hasMore {
		page = page[:limit]
	}
	data := make([]paylinks.View, 0, len(page))
	for _, l := range page {
		data = append(data, paylinks.ViewLink(l, a.links.URL(l)))
	}
	next := ""
	if hasMore {
		next = page[len(page)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": hasMore, "next_cursor": next})
}

func (a *API) deactivatePaymentLink(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.linksEnabled(w, r) || !a.allow(w, r, pr, authz.PaymentCreate) {
		return
	}
	l, err := a.links.Deactivate(r.Context(), auditActor(pr), scope(pr), r.PathValue("id"))
	if err != nil {
		a.failLink(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, paylinks.ViewLink(l, a.links.URL(l)))
}
