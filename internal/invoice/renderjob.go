package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/invoice/render"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// exportEndorsement is the statutory text printed on exports under LUT
// (spec section 10). It belongs to template version v1.
const exportEndorsement = "SUPPLY MEANT FOR EXPORT/SUPPLY TO SEZ UNIT OR SEZ DEVELOPER FOR AUTHORISED OPERATIONS UNDER BOND OR LETTER OF UNDERTAKING WITHOUT PAYMENT OF INTEGRATED TAX"

// RegisterJobs registers the invoice.render and invoice.alert handlers on q.
func (s *Service) RegisterJobs(q *jobs.Queue) {
	q.Register(JobRender, s.handleRender)
	q.Register(JobAlert, s.handleAlert)
}

type renderPayload struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// docParts is everything needed to render and deliver one document.
type docParts struct {
	doc       render.Document
	number    string
	pdfBlobID string
	paymentID string
	appID     string
	pending   bool // INR value not yet known (fx_pending)
}

func (s *Service) handleRender(ctx context.Context, raw json.RawMessage) error {
	var pl renderPayload
	if err := json.Unmarshal(raw, &pl); err != nil || (pl.Kind != kindInvoice && pl.Kind != kindCreditNote) || pl.ID == "" {
		return jobs.Permanent(fmt.Errorf("invoice.render: bad payload %s", raw))
	}
	var parts docParts
	var customer store.Customer
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if parts, err = s.loadParts(ctx, q, pl.Kind, pl.ID); err != nil {
			return err
		}
		p, err := store.GetPayment(ctx, q, parts.paymentID)
		if err != nil {
			return err
		}
		if p.CustomerID != "" {
			if customer, err = store.GetCustomer(ctx, q, p.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(fmt.Errorf("invoice.render: %s %s not found", pl.Kind, pl.ID))
	}
	if err != nil {
		return err
	}
	if parts.pending {
		// SetFXRate enqueues the render once the rate is entered.
		return nil
	}

	pdf, err := s.pdfFor(ctx, pl.Kind, pl.ID, &parts)
	if err != nil {
		return err
	}
	return s.deliver(ctx, pl.Kind, pl.ID, parts, customer, pdf)
}

// pdfFor returns the stored PDF, rendering and storing it first if needed.
func (s *Service) pdfFor(ctx context.Context, kind, id string, parts *docParts) ([]byte, error) {
	if parts.pdfBlobID != "" {
		var b store.Blob
		if err := s.db.Read(ctx, func(q store.Querier) (err error) { b, err = store.GetBlob(ctx, q, parts.pdfBlobID); return }); err != nil {
			return nil, err
		}
		return b.Data, nil
	}
	pdf, err := render.Render(parts.doc, render.Options{})
	if err != nil && len(parts.doc.Supplier.Logo) > 0 {
		// A broken logo must not stop the invoice: render without it.
		d := parts.doc
		d.Supplier.Logo = nil
		pdf, err = render.Render(d, render.Options{})
	}
	if err != nil {
		return nil, jobs.Permanent(err)
	}
	sum := sha256.Sum256(pdf)
	blob := store.Blob{ID: ids.New("blob"), OrgID: store.DefaultOrgID, Kind: kind + "_pdf", ContentType: "application/pdf",
		SHA256: hex.EncodeToString(sum[:]), Data: pdf, CreatedAt: s.now().UTC()}
	err = s.db.Write(ctx, func(tx store.Tx) error {
		// Another worker may have stored it meanwhile: keep the first.
		cur, err := s.currentBlobID(ctx, tx, kind, id)
		if err != nil {
			return err
		}
		if cur != "" {
			return nil
		}
		if err := store.InsertBlob(ctx, tx, blob); err != nil {
			return err
		}
		if kind == kindInvoice {
			return store.SetInvoicePDF(ctx, tx, id, blob.ID)
		}
		return store.SetCreditNotePDF(ctx, tx, id, blob.ID)
	})
	return pdf, err
}

func (s *Service) currentBlobID(ctx context.Context, q store.Queryer, kind, id string) (string, error) {
	if kind == kindInvoice {
		i, err := store.GetInvoice(ctx, q, id)
		return i.PDFBlobID, err
	}
	c, err := store.GetCreditNote(ctx, q, id)
	return c.PDFBlobID, err
}

// deliver emails the PDF when invoice_delivery is paymux or both and the
// customer has an email; each attempt is recorded, and a document that was
// already sent is not sent again.
func (s *Service) deliver(ctx context.Context, kind, id string, parts docParts, c store.Customer, pdf []byte) error {
	v, err := s.settings.Resolve(ctx, parts.appID, "invoice_delivery")
	if err != nil {
		return err
	}
	var mode string
	if err := v.Decode(&mode); err != nil {
		return err
	}
	if mode != "paymux" && mode != "both" {
		return nil
	}
	if strings.TrimSpace(c.Email) == "" {
		return nil
	}
	var sends []store.DocumentSend
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { sends, err = store.DocumentSends(ctx, q, kind, id); return }); err != nil {
		return err
	}
	for _, d := range sends {
		if d.Status == "sent" {
			return nil
		}
	}
	return s.send(ctx, kind, id, parts, c, pdf)
}

// send emails the PDF to the customer and records the attempt. A failed send
// is recorded and returned.
func (s *Service) send(ctx context.Context, kind, id string, parts docParts, c store.Customer, pdf []byte) error {
	title, verb := "Tax invoice", "invoice"
	if kind == kindCreditNote {
		title, verb = "Credit note", "credit note"
	}
	supplier := firstNonEmpty(parts.doc.Supplier.TradeName, parts.doc.Supplier.LegalName)
	msg := notify.Message{
		To:      []string{c.Email},
		Subject: fmt.Sprintf("%s %s from %s", title, parts.number, supplier),
		Text: fmt.Sprintf("Hello %s,\n\nPlease find your %s %s from %s attached as a PDF.\n\nThis is a computer-generated document.\n",
			firstNonEmpty(c.Name, "customer"), verb, parts.number, supplier),
		Attachments: []notify.Attachment{{Filename: strings.ReplaceAll(parts.number, "/", "-") + ".pdf", ContentType: "application/pdf", Data: pdf}},
	}
	sendErr := s.mailer.Send(ctx, msg)
	rec := store.DocumentSend{ID: ids.New("snd"), Kind: kind, DocID: id, Recipient: c.Email, Status: "sent", SentAt: s.now().UTC()}
	if sendErr != nil {
		rec.Status, rec.Error = "failed", sendErr.Error()
	}
	if err := s.db.Write(ctx, func(tx store.Tx) error { return store.InsertDocumentSend(ctx, tx, rec) }); err != nil {
		return err
	}
	return sendErr // a failed send retries the job; the PDF is already stored
}

// ResendDocument emails an invoice or credit note to its customer again, at an
// operator's request: whatever the invoice_delivery mode, and however often it
// was sent before. It returns the address it was sent to and audits the action.
func (s *Service) ResendDocument(ctx context.Context, actor audit.Actor, kind, id string) (string, error) {
	if kind != kindInvoice && kind != kindCreditNote {
		return "", invalid("kind %q must be invoice or credit_note", kind)
	}
	var parts docParts
	var customer store.Customer
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if parts, err = s.loadParts(ctx, q, kind, id); err != nil {
			return err
		}
		p, err := store.GetPayment(ctx, q, parts.paymentID)
		if err != nil {
			return err
		}
		if p.CustomerID != "" {
			if customer, err = store.GetCustomer(ctx, q, p.CustomerID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", invalid("no %s %q", kind, id)
	}
	if err != nil {
		return "", err
	}
	if parts.pending {
		return "", invalid("the INR value of %s is pending: enter the FX rate for its day first", parts.number)
	}
	if strings.TrimSpace(customer.Email) == "" {
		return "", invalid("the customer of %s has no email address", parts.number)
	}
	pdf, err := s.pdfFor(ctx, kind, id, &parts)
	if err != nil {
		return "", err
	}
	sendErr := s.send(ctx, kind, id, parts, customer, pdf)
	status := "sent"
	if sendErr != nil {
		status = "failed"
	}
	auditErr := s.db.Write(ctx, func(tx store.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{OrgID: store.DefaultOrgID, Actor: actor, Action: kind + ".resend", Target: id,
			After: map[string]string{"to": customer.Email, "status": status}})
	})
	if sendErr != nil {
		return customer.Email, sendErr
	}
	return customer.Email, auditErr
}

// loadParts reads a document and builds the render.Document for it.
func (s *Service) loadParts(ctx context.Context, q store.Queryer, kind, id string) (docParts, error) {
	if kind == kindInvoice {
		inv, err := store.GetInvoice(ctx, q, id)
		if err != nil {
			return docParts{}, err
		}
		return s.invoiceParts(ctx, q, inv)
	}
	cn, err := store.GetCreditNote(ctx, q, id)
	if err != nil {
		return docParts{}, err
	}
	inv, err := store.GetInvoice(ctx, q, cn.InvoiceID)
	if err != nil {
		return docParts{}, err
	}
	base, err := s.invoiceParts(ctx, q, inv)
	if err != nil {
		return docParts{}, err
	}
	d := base.doc
	d.Kind, d.Number, d.IssueDate = render.KindCreditNote, cn.Number, cn.IssuedAt
	d.OriginalNumber, d.OriginalDate, d.Reason = inv.Number, inv.IssuedAt, cn.Reason
	d.TemplateVersion = cn.TemplateVersion
	lines, totals, err := docLines(cn.Lines, cn.Currency, inv.FXRate, amountsOf(cn.Taxable, cn.CGST, cn.SGST, cn.IGST, cn.Total))
	if err != nil {
		return docParts{}, err
	}
	d.Lines, d.Totals = lines, totals
	d.ForeignAmount = cn.Total
	return docParts{doc: d, number: cn.Number, pdfBlobID: cn.PDFBlobID, paymentID: cn.PaymentID, appID: cn.AppID, pending: base.pending}, nil
}

func (s *Service) invoiceParts(ctx context.Context, q store.Queryer, inv store.Invoice) (docParts, error) {
	var sup Supplier
	var rcp Recipient
	if err := json.Unmarshal(inv.Supplier, &sup); err != nil {
		return docParts{}, err
	}
	if err := json.Unmarshal(inv.Recipient, &rcp); err != nil {
		return docParts{}, err
	}
	var logo []byte
	if sup.LogoBlobID != "" {
		if b, err := store.GetBlob(ctx, q, sup.LogoBlobID); err == nil {
			logo = b.Data
		}
	}
	pos := "Outside India"
	if inv.PlaceOfSupply != tax.PlaceOutsideIndia {
		name, _ := tax.StateName(inv.PlaceOfSupply)
		pos = fmt.Sprintf("%s (%s)", name, inv.PlaceOfSupply)
	} else if rcp.Country != "" {
		pos = fmt.Sprintf("Outside India (%s)", firstNonEmpty(rcp.CountryName, rcp.Country))
	}
	lines, totals, err := docLines(inv.Lines, inv.Currency, inv.FXRate, amountsOf(inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total))
	if err != nil {
		return docParts{}, err
	}
	d := render.Document{
		Kind: render.KindInvoice, Number: inv.Number, IssueDate: inv.IssuedAt,
		Supplier: render.Party{LegalName: sup.LegalName, TradeName: sup.TradeName, Address: sup.Address, GSTIN: sup.GSTIN,
			StateName: sup.StateName, StateCode: sup.StateCode, Logo: logo},
		Recipient: render.Party{Name: rcp.Name, Address: rcp.Address, GSTIN: rcp.GSTIN, StateName: rcp.StateName,
			StateCode: rcp.StateCode, Country: rcp.Country, CountryName: firstNonEmpty(rcp.CountryName, rcp.Country)},
		PlaceOfSupply: pos, SupplyType: tax.SupplyType(inv.SupplyType), InterState: inv.InterState,
		Lines: lines, Totals: totals, PaymentMethod: inv.PaymentMethod, PaymentRef: inv.PaymentRef,
		SignatoryName: sup.SignatoryName, FooterNote: sup.FooterNote, TemplateVersion: inv.TemplateVersion,
	}
	if inv.SupplyType == string(tax.ExportLUT) {
		d.ExportEndorsement, d.LUTNumber = exportEndorsement, inv.LUTNumber
	}
	if inv.Currency != "INR" {
		d.Currency, d.ForeignAmount, d.FXRate = inv.Currency, inv.Total, inv.FXRate
	}
	if inv.IRN != "" {
		d.EInvoice = &render.EInvoiceInfo{IRN: inv.IRN, AckNo: inv.AckNo, AckDate: inv.AckDate, QR: inv.SignedQR}
	}
	return docParts{doc: d, number: inv.Number, pdfBlobID: inv.PDFBlobID, paymentID: inv.PaymentID, appID: inv.AppID,
		pending: inv.Currency != "INR" && inv.FXRate == ""}, nil
}

// docLines turns stored lines (document currency) into render lines in paise,
// converting at the invoice's FX rate for foreign currencies. Totals is the
// sum of the converted lines.
func docLines(raw json.RawMessage, currency, rate string, fallback tax.Amounts) ([]render.Line, tax.Amounts, error) {
	var in []Line
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, tax.Amounts{}, err
	}
	if currency != "INR" && rate == "" {
		return nil, fallback, nil // pending: caller skips rendering
	}
	var out []render.Line
	var sum tax.Amounts
	for _, l := range in {
		a, _, err := inrAmounts(tax.Amounts{Taxable: l.Taxable, CGST: l.CGST, SGST: l.SGST, IGST: l.IGST}, currency, rate)
		if err != nil {
			return nil, tax.Amounts{}, err
		}
		out = append(out, render.Line{Description: l.Description, SAC: l.SAC, Qty: l.Qty, Unit: l.Unit, Taxable: a.Taxable,
			CGSTRateBP: l.CGSTRateBP, SGSTRateBP: l.SGSTRateBP, IGSTRateBP: l.IGSTRateBP, CGST: a.CGST, SGST: a.SGST, IGST: a.IGST})
		sum = sum.Add(tax.Amounts{Taxable: a.Taxable, CGST: a.CGST, SGST: a.SGST, IGST: a.IGST,
			Total: a.Taxable + a.CGST + a.SGST + a.IGST})
	}
	return out, sum, nil
}
