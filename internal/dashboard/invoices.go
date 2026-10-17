package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// docFlags are the flags the invoice list can filter on.
var docFlags = []string{tax.FlagPOSDefaulted, invoice.FlagFXPending, tax.FlagExportIGST, invoice.FlagRecipientInvalid,
	invoice.FlagReissued, invoice.FlagCutoffPassed}

type docFilter struct{ Entity, App, Type, Month, Flag string }

type docRow struct {
	ID, Href, Number, Date, App, Entity, Party, Total, Flags, Note string
}

type flaggedRefund struct{ RefundID, PaymentID, Flag, At string }

type docLine struct{ Description, SAC, Taxable, CGST, SGST, IGST string }

type sendRow struct{ To, Status, At, Error string }

type entityOpt struct{ ID, Name, GSTIN string }

// docView is one invoice or credit note laid out for its page.
type docView struct {
	Kind, ID, Number, Date, App, AppID, Entity, EntityID   string
	SupplyType, PlaceOfSupply, Currency, FXRate, PaymentID string
	Supplier, Recipient                                    invoice.Supplier
	RecipientView                                          invoice.Recipient
	Lines                                                  []docLine
	Taxable, CGST, SGST, IGST, Total                       string
	Flags                                                  []string
	PDFReady                                               bool
	PDFHref, Reason, InvoiceID, InvoiceNumber              string
	CreditNotes                                            []docRow
	Sends                                                  []sendRow
	CanResend, CanReissue                                  bool
	ReissueOpen                                            bool
	Billing                                                store.Billing
	ReissueReason                                          string
	EInvoice                                               *eInvoiceView
}

func (d *Dashboard) routesInvoices() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/invoices", d.authed(authz.PaymentRead, d.invoicesPage))
	m.HandleFunc("GET "+prefix+"/invoices/{id}", d.authed(authz.PaymentRead, d.invoicePage))
	m.HandleFunc("GET "+prefix+"/invoices/{id}/pdf", d.authed(authz.PaymentRead, d.invoicePDF))
	// Resending an email is an operator action; reissuing is admin-only, so it
	// rides on the admin-only SettingsWrite action (authz has no finer one).
	m.HandleFunc("POST "+prefix+"/invoices/{id}/resend", d.authed(authz.InvoiceReissue, d.invoiceResend))
	m.HandleFunc("POST "+prefix+"/invoices/{id}/reissue", d.authed(authz.SettingsWrite, d.invoiceReissue))
	m.HandleFunc("GET "+prefix+"/credit-notes/{id}", d.authed(authz.PaymentRead, d.creditNotePage))
	m.HandleFunc("GET "+prefix+"/credit-notes/{id}/pdf", d.authed(authz.PaymentRead, d.creditNotePDF))
	m.HandleFunc("POST "+prefix+"/credit-notes/{id}/resend", d.authed(authz.InvoiceReissue, d.creditNoteResend))
}

// monthBounds returns [first day 00:00 IST, first day of next month) in UTC.
func monthBounds(month string) (from, to time.Time, err error) {
	t, err := time.ParseInLocation("2006-01", month, tax.IST)
	if err != nil {
		return from, to, fieldErr("month must look like 2026-10")
	}
	return t.UTC(), t.AddDate(0, 1, 0).UTC(), nil
}

func (d *Dashboard) entityOpts(r *http.Request) ([]entityOpt, error) {
	var out []entityOpt
	err := d.o.DB.Read(r.Context(), func(q store.Querier) error {
		org, err := store.FirstOrgID(r.Context(), q)
		if err != nil {
			return err
		}
		ents, err := store.ListTaxEntities(r.Context(), q, org)
		for _, e := range ents {
			out = append(out, entityOpt{ID: e.ID, Name: firstNonBlank(e.TradeName, e.LegalName), GSTIN: e.GSTIN})
		}
		return err
	})
	return out, err
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func (d *Dashboard) invoicesPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	qv := r.URL.Query()
	f := docFilter{Entity: qv.Get("entity"), App: qv.Get("app"), Type: qv.Get("type"), Month: qv.Get("month"), Flag: qv.Get("flag")}
	if f.Type == "" {
		f.Type = "invoice"
	}
	if f.Type != "invoice" && f.Type != "credit_note" {
		badRequest(w, "type must be invoice or credit_note")
		return
	}
	if f.Flag != "" && !slices.Contains(docFlags, f.Flag) {
		badRequest(w, "unknown flag")
		return
	}
	org, err := d.orgID(ctx)
	if err != nil {
		d.fail(w, err)
		return
	}
	sf := store.DocFilter{OrgID: org, EntityID: f.Entity, AppID: f.App, Mode: d.mode(r, s), Flag: f.Flag}
	if f.Month != "" {
		var err error
		if sf.From, sf.To, err = monthBounds(f.Month); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	ents, err := d.entityOpts(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	entName := map[string]string{}
	for _, e := range ents {
		entName[e.ID] = e.Name
	}
	if f.Entity != "" && entName[f.Entity] == "" {
		badRequest(w, "unknown tax entity")
		return
	}
	var apps []store.App
	if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) { apps, err = store.ListApps(ctx, q, org); return }); err != nil {
		d.fail(w, err)
		return
	}
	appName := map[string]string{}
	v := d.shell(r, s, "invoices", "Invoices")
	for _, a := range apps {
		appName[a.ID] = a.Name
		v.AppOpts = append(v.AppOpts, appOpt{ID: a.ID, Name: a.Name})
	}
	v.Entities, v.DocFlags, v.DocFilter = ents, docFlags, f

	if f.Flag == invoice.FlagCutoffPassed {
		// This flag sits on refunds whose credit note could no longer be issued.
		from, to := sf.From, sf.To
		if from.IsZero() {
			from, to = time.Unix(0, 0), d.o.Now().Add(24*time.Hour)
		}
		err = d.o.DB.Read(ctx, func(q store.Querier) error {
			for _, e := range ents {
				if f.Entity != "" && e.ID != f.Entity {
					continue
				}
				flags, err := store.RefundFlagsInRange(ctx, q, e.ID, from, to)
				if err != nil {
					return err
				}
				for _, fl := range flags {
					if fl.Flag != f.Flag {
						continue
					}
					rf, err := store.GetRefund(ctx, q, fl.RefundID)
					if err != nil {
						return err
					}
					if rf.Mode == sf.Mode {
						v.FlaggedRefunds = append(v.FlaggedRefunds, flaggedRefund{RefundID: rf.ID, PaymentID: rf.PaymentID, Flag: fl.Flag, At: d.fmtTime(fl.CreatedAt)})
					}
				}
			}
			return nil
		})
		if err != nil {
			d.fail(w, err)
			return
		}
		d.render(w, http.StatusOK, "dash_invoices", v)
		return
	}

	after := qv.Get("after")
	var lastID string
	more := false
	if f.Type == "invoice" {
		var rows []store.Invoice
		if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
			rows, err = store.SearchInvoices(ctx, q, sf, after, d.o.PageSize+1)
			return
		}); err != nil {
			d.fail(w, err)
			return
		}
		if more = len(rows) > d.o.PageSize; more {
			rows = rows[:d.o.PageSize]
		}
		for _, i := range rows {
			var rcp invoice.Recipient
			_ = json.Unmarshal(i.Recipient, &rcp)
			v.DocRows = append(v.DocRows, docRow{ID: i.ID, Href: prefix + "/invoices/" + i.ID, Number: i.Number, Date: d.fmtTime(i.IssuedAt),
				App: appName[i.AppID], Entity: entName[i.TaxEntityID], Party: rcp.Name, Total: fmtMoney(i.Total, i.Currency), Flags: strings.Join(i.Flags, ", ")})
			lastID = i.ID
		}
	} else {
		var rows []store.CreditNote
		if err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
			rows, err = store.SearchCreditNotes(ctx, q, sf, after, d.o.PageSize+1)
			return
		}); err != nil {
			d.fail(w, err)
			return
		}
		if more = len(rows) > d.o.PageSize; more {
			rows = rows[:d.o.PageSize]
		}
		for _, c := range rows {
			v.DocRows = append(v.DocRows, docRow{ID: c.ID, Href: prefix + "/credit-notes/" + c.ID, Number: c.Number, Date: d.fmtTime(c.IssuedAt),
				App: appName[c.AppID], Entity: entName[c.TaxEntityID], Total: fmtMoney(c.Total, c.Currency), Flags: strings.Join(c.Flags, ", "), Note: c.Reason})
			lastID = c.ID
		}
	}
	if more {
		next := url.Values{}
		for k, val := range map[string]string{"entity": f.Entity, "app": f.App, "type": f.Type, "month": f.Month, "flag": f.Flag} {
			if val != "" {
				next.Set(k, val)
			}
		}
		next.Set("after", lastID)
		v.NextURL = prefix + "/invoices?" + next.Encode()
	}
	d.render(w, http.StatusOK, "dash_invoices", v)
}

// docMessage is what a document page shows after an action.
type docMessage struct {
	status  int
	err     string
	notice  string
	reissue bool
	billing store.Billing
	reason  string
	// the e-invoice record form as typed, after a mistake
	einv     invoice.EInvoiceRecord
	einvOpen bool
}

func (d *Dashboard) invoicePage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderInvoice(w, r, s, docMessage{status: http.StatusOK})
}

func (d *Dashboard) renderInvoice(w http.ResponseWriter, r *http.Request, s *session, m docMessage) {
	ctx := r.Context()
	var inv store.Invoice
	var cns []store.CreditNote
	var sends []store.DocumentSend
	var ent store.TaxEntity
	var app store.App
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if inv, err = store.GetInvoice(ctx, q, r.PathValue("id")); err != nil {
			return err
		}
		if cns, err = store.CreditNotesForInvoice(ctx, q, inv.ID); err != nil {
			return err
		}
		if sends, err = store.DocumentSends(ctx, q, "invoice", inv.ID); err != nil {
			return err
		}
		if ent, err = store.GetTaxEntity(ctx, q, inv.TaxEntityID); err != nil {
			return err
		}
		app, err = store.GetApp(ctx, q, inv.AppID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(ctx, inv.OrgID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	doc := &docView{Kind: "invoice", ID: inv.ID, Number: inv.Number, Date: d.fmtTime(inv.IssuedAt), App: app.Name, AppID: app.ID,
		Entity: firstNonBlank(ent.TradeName, ent.LegalName), EntityID: ent.ID, SupplyType: inv.SupplyType, PlaceOfSupply: inv.PlaceOfSupply,
		Currency: inv.Currency, FXRate: inv.FXRate, PaymentID: inv.PaymentID, Flags: inv.Flags, PDFReady: inv.PDFBlobID != "",
		PDFHref: prefix + "/invoices/" + inv.ID + "/pdf", CanResend: s.can(authz.InvoiceReissue), CanReissue: s.can(authz.SettingsWrite) && len(cns) == 0}
	_ = json.Unmarshal(inv.Supplier, &doc.Supplier)
	_ = json.Unmarshal(inv.Recipient, &doc.RecipientView)
	d.fillTotals(doc, inv.Lines, inv.Currency, inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total)
	for _, c := range cns {
		doc.CreditNotes = append(doc.CreditNotes, docRow{ID: c.ID, Href: prefix + "/credit-notes/" + c.ID, Number: c.Number, Date: d.fmtTime(c.IssuedAt),
			Total: fmtMoney(c.Total, c.Currency), Note: c.Reason})
	}
	for _, sd := range sends {
		doc.Sends = append(doc.Sends, sendRow{To: sd.Recipient, Status: sd.Status, At: d.fmtTime(sd.SentAt), Error: sd.Error})
	}
	// Prefill the reissue form with what the invoice says now.
	doc.Billing = store.Billing{LegalName: doc.RecipientView.Name, GSTIN: doc.RecipientView.GSTIN, StateCode: doc.RecipientView.StateCode,
		Country: doc.RecipientView.Country}
	if m.reissue {
		doc.Billing, doc.ReissueReason, doc.ReissueOpen = m.billing, m.reason, true
	}
	d.fillEInvoice(doc, inv, s, m)
	v := d.shell(r, s, "invoices", "Invoice "+inv.Number)
	v.Doc, v.Error, v.Notice = doc, m.err, m.notice
	d.render(w, firstStatus(m.status), "dash_invoice", v)
}

func firstStatus(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

func (d *Dashboard) fillTotals(doc *docView, rawLines json.RawMessage, cur string, taxable, cgst, sgst, igst, total int64) {
	var lines []invoice.Line
	_ = json.Unmarshal(rawLines, &lines)
	for _, l := range lines {
		doc.Lines = append(doc.Lines, docLine{Description: l.Description, SAC: l.SAC, Taxable: fmtMoney(l.Taxable, cur),
			CGST: fmtMoney(l.CGST, cur), SGST: fmtMoney(l.SGST, cur), IGST: fmtMoney(l.IGST, cur)})
	}
	doc.Taxable, doc.CGST, doc.SGST, doc.IGST, doc.Total = fmtMoney(taxable, cur), fmtMoney(cgst, cur), fmtMoney(sgst, cur), fmtMoney(igst, cur), fmtMoney(total, cur)
}

func (d *Dashboard) servePDF(w http.ResponseWriter, r *http.Request, blobID, number string) {
	if blobID == "" {
		http.Error(w, "The PDF is not ready yet.", http.StatusNotFound)
		return
	}
	pdf, err := d.o.Invoice.PDF(r.Context(), blobID)
	if err != nil {
		d.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.pdf"`, strings.ReplaceAll(number, "/", "-")))
	_, _ = w.Write(pdf)
}

func (d *Dashboard) invoicePDF(w http.ResponseWriter, r *http.Request, s *session) {
	var inv store.Invoice
	err := d.o.DB.Read(r.Context(), func(q store.Querier) (err error) {
		inv, err = store.GetInvoice(r.Context(), q, r.PathValue("id"))
		return
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(r.Context(), inv.OrgID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	d.servePDF(w, r, inv.PDFBlobID, inv.Number)
}

func (d *Dashboard) creditNotePage(w http.ResponseWriter, r *http.Request, s *session) {
	d.renderCreditNote(w, r, s, docMessage{status: http.StatusOK})
}

func (d *Dashboard) renderCreditNote(w http.ResponseWriter, r *http.Request, s *session, m docMessage) {
	ctx := r.Context()
	var cn store.CreditNote
	var inv store.Invoice
	var sends []store.DocumentSend
	var ent store.TaxEntity
	var app store.App
	err := d.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if cn, err = store.GetCreditNote(ctx, q, r.PathValue("id")); err != nil {
			return err
		}
		if inv, err = store.GetInvoice(ctx, q, cn.InvoiceID); err != nil {
			return err
		}
		if sends, err = store.DocumentSends(ctx, q, "credit_note", cn.ID); err != nil {
			return err
		}
		if ent, err = store.GetTaxEntity(ctx, q, cn.TaxEntityID); err != nil {
			return err
		}
		app, err = store.GetApp(ctx, q, cn.AppID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && !d.inOrg(ctx, cn.OrgID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	doc := &docView{Kind: "credit_note", ID: cn.ID, Number: cn.Number, Date: d.fmtTime(cn.IssuedAt), App: app.Name, AppID: app.ID,
		Entity: firstNonBlank(ent.TradeName, ent.LegalName), EntityID: ent.ID, Currency: cn.Currency, PaymentID: cn.PaymentID, Flags: cn.Flags,
		PDFReady: cn.PDFBlobID != "", PDFHref: prefix + "/credit-notes/" + cn.ID + "/pdf", Reason: cn.Reason, InvoiceID: inv.ID, InvoiceNumber: inv.Number,
		CanResend: s.can(authz.InvoiceReissue)}
	d.fillTotals(doc, cn.Lines, cn.Currency, cn.Taxable, cn.CGST, cn.SGST, cn.IGST, cn.Total)
	for _, sd := range sends {
		doc.Sends = append(doc.Sends, sendRow{To: sd.Recipient, Status: sd.Status, At: d.fmtTime(sd.SentAt), Error: sd.Error})
	}
	v := d.shell(r, s, "invoices", "Credit note "+cn.Number)
	v.Doc, v.Error, v.Notice = doc, m.err, m.notice
	d.render(w, firstStatus(m.status), "dash_invoice", v)
}

func (d *Dashboard) creditNotePDF(w http.ResponseWriter, r *http.Request, s *session) {
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
	d.servePDF(w, r, cn.PDFBlobID, cn.Number)
}

// resend emails a document again; kind is invoice or credit_note.
func (d *Dashboard) resend(w http.ResponseWriter, r *http.Request, s *session, kind string) {
	id := r.PathValue("id")
	render := d.renderInvoice
	exists := func(q store.Querier) error {
		i, err := store.GetInvoice(r.Context(), q, id)
		if err == nil && !d.inOrg(r.Context(), i.OrgID) {
			return store.ErrNotFound
		}
		return err
	}
	if kind == "credit_note" {
		render = d.renderCreditNote
		exists = func(q store.Querier) error {
			c, err := store.GetCreditNote(r.Context(), q, id)
			if err == nil && !d.inOrg(r.Context(), c.OrgID) {
				return store.ErrNotFound
			}
			return err
		}
	}
	if err := d.o.DB.Read(r.Context(), exists); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		d.fail(w, err)
		return
	}
	_, err := d.o.Invoice.ResendDocument(r.Context(), s.auditActor(), kind, id)
	switch {
	case err == nil:
		base := "/invoices/"
		if kind == "credit_note" {
			base = "/credit-notes/"
		}
		http.Redirect(w, r, prefix+base+id, http.StatusSeeOther)
	case errors.Is(err, invoice.ErrInvalid):
		render(w, r, s, docMessage{status: http.StatusBadRequest, err: strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": ")})
	default:
		render(w, r, s, docMessage{status: http.StatusBadGateway, err: "The email could not be sent: " + err.Error()})
	}
}

func (d *Dashboard) invoiceResend(w http.ResponseWriter, r *http.Request, s *session) {
	d.resend(w, r, s, "invoice")
}

func (d *Dashboard) creditNoteResend(w http.ResponseWriter, r *http.Request, s *session) {
	d.resend(w, r, s, "credit_note")
}

func (d *Dashboard) invoiceReissue(w http.ResponseWriter, r *http.Request, s *session) {
	id := r.PathValue("id")
	if err := d.o.DB.Read(r.Context(), func(q store.Querier) error {
		i, err := store.GetInvoice(r.Context(), q, id)
		if err == nil && !d.inOrg(r.Context(), i.OrgID) {
			return store.ErrNotFound
		}
		return err
	}); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		d.fail(w, err)
		return
	}
	f := r.PostForm
	b := store.Billing{LegalName: strings.TrimSpace(f.Get("legal_name")), GSTIN: strings.ToUpper(strings.TrimSpace(f.Get("gstin"))),
		StateCode: strings.TrimSpace(f.Get("state_code")), Line1: strings.TrimSpace(f.Get("line1")), Line2: strings.TrimSpace(f.Get("line2")),
		City: strings.TrimSpace(f.Get("city")), PostalCode: strings.TrimSpace(f.Get("postal_code")), Country: strings.ToUpper(strings.TrimSpace(f.Get("country")))}
	reason := strings.TrimSpace(f.Get("reason"))
	res, err := d.o.Invoice.ReissueInvoice(r.Context(), s.auditActor(), id, b, reason)
	switch {
	case err == nil:
		http.Redirect(w, r, prefix+"/invoices/"+res.Invoice.ID, http.StatusSeeOther)
	case errors.Is(err, invoice.ErrInvalid):
		d.renderInvoice(w, r, s, docMessage{status: http.StatusBadRequest, err: strings.TrimPrefix(err.Error(), invoice.ErrInvalid.Error()+": "),
			reissue: true, billing: b, reason: reason})
	default:
		d.fail(w, err)
	}
}
