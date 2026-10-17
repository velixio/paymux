package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/invoice/einvoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// E-invoice (offline mode): download the JSON for the Invoice Registration
// Portal, upload it there, and record the IRN, acknowledgement and signed QR.

// eInvoiceView is the e-invoice section of an invoice page.
type eInvoiceView struct {
	JSONHref  string
	CanRecord bool
	Recorded  bool
	IRN       string
	AckNo     string
	AckDate   string
	At        string
	// The record form as typed, shown again after a mistake.
	Form invoice.EInvoiceRecord
}

type eInvoiceRow struct {
	Number, Buyer, Total, Status string
	Href                         string
	Problems                     []string
}

// eInvoiceReport is the e-invoice block of the reports page.
type eInvoiceReport struct {
	Ready, Done, Problems, SkippedB2C int
	Rows                              []eInvoiceRow
	ReadyHref, AllHref                string
}

func (d *Dashboard) routesEInvoice() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/invoices/{id}/einvoice.json", d.authed(authz.PaymentRead, d.invoiceEInvoiceJSON))
	// Recording what the portal returned is an operator action, like resending.
	m.HandleFunc("POST "+prefix+"/invoices/{id}/einvoice", d.authed(authz.InvoiceReissue, d.invoiceEInvoiceRecord))
	m.HandleFunc("GET "+prefix+"/reports/einvoice.json", d.authed(authz.PaymentRead, d.reportEInvoiceJSON))
}

func (d *Dashboard) loadInvoiceInOrg(w http.ResponseWriter, r *http.Request) (store.Invoice, bool) {
	var inv store.Invoice
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		inv, err = store.GetInvoice(r.Context(), q, r.PathValue("id"))
		return
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(r.Context(), inv.OrgID) {
		http.NotFound(w, r)
		return inv, false
	}
	if err != nil {
		d.fail(w, err)
		return inv, false
	}
	return inv, true
}

func trimInvalid(err error) string {
	return strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": ")
}

func (d *Dashboard) invoiceEInvoiceJSON(w http.ResponseWriter, r *http.Request, s *session) {
	inv, ok := d.loadInvoiceInOrg(w, r)
	if !ok {
		return
	}
	res, err := d.o.Invoice.EInvoice(r.Context(), inv.ID)
	var pe *einvoice.Problems
	switch {
	case errors.As(err, &pe):
		d.renderInvoice(w, r, s, docMessage{status: http.StatusUnprocessableEntity,
			err: "The e-invoice cannot be built yet: " + strings.Join(pe.List, "; ") + "."})
		return
	case errors.Is(err, invoice.ErrInvalid):
		d.renderInvoice(w, r, s, docMessage{status: http.StatusBadRequest, err: trimInvalid(err)})
		return
	case err != nil:
		d.fail(w, err)
		return
	}
	d.sendJSON(w, res.Filename, res.JSON)
}

// creditNoteEInvoiceJSON downloads the CRN e-invoice of a credit note of the
// caller's org.
func (d *Dashboard) creditNoteEInvoiceJSON(w http.ResponseWriter, r *http.Request, s *session) {
	var cn store.CreditNote
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		cn, err = store.GetCreditNote(r.Context(), q, r.PathValue("id"))
		return
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(r.Context(), cn.OrgID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	res, err := d.o.Invoice.CreditNoteEInvoice(r.Context(), cn.ID)
	var pe *einvoice.Problems
	switch {
	case errors.As(err, &pe):
		http.Error(w, "The e-invoice cannot be built yet: "+strings.Join(pe.List, "; ")+".", http.StatusUnprocessableEntity)
		return
	case errors.Is(err, invoice.ErrInvalid):
		badRequest(w, trimInvalid(err))
		return
	case err != nil:
		d.fail(w, err)
		return
	}
	d.sendJSON(w, res.Filename, res.JSON)
}

func (d *Dashboard) sendJSON(w http.ResponseWriter, filename string, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(body)
}

func (d *Dashboard) invoiceEInvoiceRecord(w http.ResponseWriter, r *http.Request, s *session) {
	inv, ok := d.loadInvoiceInOrg(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	in := invoice.EInvoiceRecord{IRN: f.Get("irn"), AckNo: f.Get("ack_no"), AckDate: f.Get("ack_date"), SignedQR: f.Get("signed_qr")}
	if _, err := d.o.Invoice.RecordEInvoice(r.Context(), s.auditActor(), inv.ID, in); err != nil {
		if errors.Is(err, invoice.ErrInvalid) {
			d.renderInvoice(w, r, s, docMessage{status: http.StatusBadRequest, err: trimInvalid(err), einv: in, einvOpen: true})
			return
		}
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/invoices/"+inv.ID, http.StatusSeeOther)
}

// fillEInvoice adds the e-invoice section to an invoice page of a B2B or export invoice.
func (d *Dashboard) fillEInvoice(doc *docView, inv store.Invoice, s *session, m docMessage) {
	if !einvoice.Eligible(inv.SupplyType) {
		return
	}
	ev := &eInvoiceView{JSONHref: prefix + "/invoices/" + inv.ID + "/einvoice.json", CanRecord: s.can(authz.InvoiceReissue), Recorded: inv.IRN != "",
		IRN: inv.IRN, AckNo: inv.AckNo}
	if inv.IRN != "" {
		ev.AckDate, ev.At = inv.AckDate.In(tax.IST).Format("2006-01-02 15:04:05"), d.fmtTime(inv.EInvoiceAt)
	}
	if m.einvOpen {
		ev.Form = m.einv
	}
	doc.EInvoice = ev
}

// reportEInvoice builds the e-invoice block for the reports page.
func (d *Dashboard) reportEInvoice(r *http.Request, entityID, month string) (*eInvoiceReport, error) {
	x, err := d.o.Invoice.EInvoiceMonth(r.Context(), entityID, month, false)
	if err != nil {
		return nil, err
	}
	rep := &eInvoiceReport{Ready: x.Ready, Done: x.Done, Problems: x.Problems, SkippedB2C: x.SkippedB2C}
	for _, row := range x.Rows {
		rep.Rows = append(rep.Rows, eInvoiceRow{Number: row.Invoice.Number, Buyer: row.Buyer, Total: fmtMoney(row.Invoice.Total, row.Invoice.Currency),
			Status: row.Status, Problems: row.Problems, Href: prefix + "/invoices/" + row.Invoice.ID})
	}
	q := url.Values{"entity": {entityID}, "month": {month}}
	rep.ReadyHref = prefix + "/reports/einvoice.json?" + q.Encode()
	q.Set("all", "1")
	rep.AllHref = prefix + "/reports/einvoice.json?" + q.Encode()
	return rep, nil
}

func (d *Dashboard) reportEInvoiceJSON(w http.ResponseWriter, r *http.Request, s *session) {
	ent, month, err := d.reportRequest(r)
	var fe fieldErr
	if errors.As(err, &fe) {
		badRequest(w, err.Error())
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	x, err := d.o.Invoice.EInvoiceMonth(r.Context(), ent.ID, month, r.URL.Query().Get("all") == "1")
	if errors.Is(err, invoice.ErrInvalid) {
		badRequest(w, trimInvalid(err))
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	d.sendJSON(w, x.Filename, x.JSON)
}
