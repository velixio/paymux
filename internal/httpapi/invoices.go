package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
)

// invoicing answers 404 when the API was built without an invoice service.
func (a *API) invoicing(w http.ResponseWriter, r *http.Request) bool {
	if a.invoices == nil {
		a.fail(w, r, http.StatusNotFound, "not_found", "invoicing is not enabled")
		return false
	}
	return true
}

func (a *API) failInvoice(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.fail(w, r, http.StatusNotFound, "not_found", "no such document")
	case errors.Is(err, invoice.ErrPDFNotReady):
		w.Header().Set("Retry-After", "5")
		a.fail(w, r, http.StatusNotFound, "not_found", "the PDF is not ready yet")
	default:
		a.serverError(w, r, err)
	}
}

func (a *API) getInvoice(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.invoicing(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	inv, err := a.invoices.GetInvoice(r.Context(), pr.AppID, pr.Mode, r.PathValue("id"))
	if err != nil {
		a.failInvoice(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, invoice.ViewInvoice(inv))
}

func (a *API) getInvoicePDF(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.invoicing(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	inv, err := a.invoices.GetInvoice(r.Context(), pr.AppID, pr.Mode, r.PathValue("id"))
	if err == nil {
		var pdf []byte
		if pdf, err = a.invoices.PDF(r.Context(), inv.PDFBlobID); err == nil {
			a.writePDF(w, inv.Number, pdf)
			return
		}
	}
	a.failInvoice(w, r, err)
}

func (a *API) listInvoices(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.invoicing(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	pid := r.URL.Query().Get("payment_id")
	if pid == "" {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "payment_id is required")
		return
	}
	list, err := a.invoices.ListInvoices(r.Context(), pr.AppID, pr.Mode, pid)
	if err != nil {
		a.failInvoice(w, r, err)
		return
	}
	data := make([]invoice.InvoiceView, 0, len(list))
	for _, i := range list {
		data = append(data, invoice.ViewInvoice(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
}

func (a *API) getCreditNote(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.invoicing(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	cn, err := a.invoices.GetCreditNote(r.Context(), pr.AppID, pr.Mode, r.PathValue("id"))
	if err != nil {
		a.failInvoice(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, invoice.ViewCreditNote(cn))
}

func (a *API) getCreditNotePDF(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.invoicing(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	cn, err := a.invoices.GetCreditNote(r.Context(), pr.AppID, pr.Mode, r.PathValue("id"))
	if err == nil {
		var pdf []byte
		if pdf, err = a.invoices.PDF(r.Context(), cn.PDFBlobID); err == nil {
			a.writePDF(w, cn.Number, pdf)
			return
		}
	}
	a.failInvoice(w, r, err)
}

func (a *API) listCreditNotes(w http.ResponseWriter, r *http.Request, pr core.Principal, _ []byte) {
	if !a.invoicing(w, r) || !a.allow(w, r, pr, authz.PaymentRead) {
		return
	}
	pid := r.URL.Query().Get("payment_id")
	if pid == "" {
		a.fail(w, r, http.StatusBadRequest, "invalid_request", "payment_id is required")
		return
	}
	list, err := a.invoices.ListCreditNotes(r.Context(), pr.AppID, pr.Mode, pid)
	if err != nil {
		a.failInvoice(w, r, err)
		return
	}
	data := make([]invoice.CreditNoteView, 0, len(list))
	for _, c := range list {
		data = append(data, invoice.ViewCreditNote(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "has_more": false})
}

func (a *API) writePDF(w http.ResponseWriter, number string, pdf []byte) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", strings.ReplaceAll(number, "/", "-")+".pdf"))
	_, _ = w.Write(pdf)
}
