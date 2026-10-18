package invoice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/invoice/einvoice"
	"github.com/paymux/paymux/internal/invoice/irp"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// GST e-invoice, offline mode: Paymux produces the portal's JSON; the operator
// uploads it to the Invoice Registration Portal and records the IRN,
// acknowledgement and signed QR code that come back.

// EInvoiceResult is one invoice's document.
type EInvoiceResult struct {
	Invoice  store.Invoice
	Doc      *einvoice.Invoice
	JSON     []byte
	Filename string
}

func (s *Service) einvoiceSource(ctx context.Context, inv store.Invoice) (einvoice.Source, error) {
	return s.documentSource(ctx, inv, inv.Number, inv.IssuedAt, inv.Lines, amountsOf(inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total))
}

// documentSource maps an invoice, or a credit note against it (its own number,
// date, lines and amounts), onto the e-invoice source. Parties and the FX rate
// are the invoice's.
func (s *Service) documentSource(ctx context.Context, inv store.Invoice, number string, issued time.Time, rawLines json.RawMessage, amounts tax.Amounts) (einvoice.Source, error) {
	var sup Supplier
	var rcp Recipient
	if err := json.Unmarshal(inv.Supplier, &sup); err != nil {
		return einvoice.Source{}, err
	}
	if err := json.Unmarshal(inv.Recipient, &rcp); err != nil {
		return einvoice.Source{}, err
	}
	if inv.Currency != "INR" && inv.FXRate == "" {
		return einvoice.Source{}, &einvoice.Problems{List: []string{fmt.Sprintf(
			"the %s FX rate for the invoice date is not set yet, so the INR values are unknown (enter it under FX rates)", inv.Currency)}}
	}
	lines, _, err := docLines(rawLines, inv.Currency, inv.FXRate, amounts)
	if err != nil {
		return einvoice.Source{}, err
	}
	src := einvoice.Source{Number: number, IssuedAt: issued, SupplyType: inv.SupplyType, PlaceOfSupply: inv.PlaceOfSupply,
		Seller: einvoice.Party{GSTIN: sup.GSTIN, LegalName: sup.LegalName, TradeName: sup.TradeName, Address: sup.Address, StateCode: sup.StateCode, Email: sup.SupportEmail},
		Buyer: einvoice.Party{GSTIN: rcp.GSTIN, LegalName: rcp.Name, Address: rcp.Address, Line1: rcp.Line1, Line2: rcp.Line2, City: rcp.City,
			PostalCode: rcp.PostalCode, StateCode: rcp.StateCode, Country: rcp.Country, CountryName: rcp.CountryName}}
	for _, l := range lines {
		src.Lines = append(src.Lines, einvoice.Line{Description: l.Description, SAC: l.SAC, Qty: l.Qty, Taxable: l.Taxable,
			CGSTRateBP: l.CGSTRateBP, SGSTRateBP: l.SGSTRateBP, IGSTRateBP: l.IGSTRateBP, CGST: l.CGST, SGST: l.SGST, IGST: l.IGST})
	}
	if inv.SupplyType != string(tax.B2B) {
		src.Currency, src.ForeignTotal = inv.Currency, amounts.Total
		src.IGSTRefundClaim = "auto"
		err := s.db.Read(ctx, func(q store.Querier) error {
			_, err := s.orgValue(ctx, q, inv.OrgID, "einvoice_igst_refund_claim", &src.IGSTRefundClaim)
			return err
		})
		if err != nil {
			return einvoice.Source{}, err
		}
	}
	return src, nil
}

func (s *Service) buildEInvoice(ctx context.Context, inv store.Invoice) (*einvoice.Invoice, error) {
	if !einvoice.Eligible(inv.SupplyType) {
		return nil, invalid("invoice %s is %s: only B2B and export invoices have an e-invoice", inv.Number, inv.SupplyType)
	}
	src, err := s.einvoiceSource(ctx, inv)
	if err != nil {
		return nil, err
	}
	return einvoice.Build(src)
}

func eInvoiceFilename(number string) string {
	return "einvoice-" + strings.NewReplacer("/", "-", " ", "-").Replace(number) + ".json"
}

// EInvoice builds, validates and renders the e-invoice JSON of one invoice.
// A document that cannot be built is returned as *einvoice.Problems, in words
// the operator can act on.
func (s *Service) EInvoice(ctx context.Context, invoiceID string) (EInvoiceResult, error) {
	var inv store.Invoice
	err := s.db.Read(ctx, func(q store.Querier) (err error) { inv, err = store.GetInvoice(ctx, q, invoiceID); return })
	if errors.Is(err, store.ErrNotFound) {
		return EInvoiceResult{}, invalid("no invoice %q", invoiceID)
	}
	if err != nil {
		return EInvoiceResult{}, err
	}
	doc, err := s.buildEInvoice(ctx, inv)
	if err != nil {
		return EInvoiceResult{}, err
	}
	raw, err := einvoice.Marshal(doc)
	if err != nil {
		return EInvoiceResult{}, err
	}
	if err := einvoice.Validate(raw); err != nil {
		return EInvoiceResult{}, &einvoice.Problems{List: []string{err.Error()}}
	}
	return EInvoiceResult{Invoice: inv, Doc: doc, JSON: raw, Filename: eInvoiceFilename(inv.Number)}, nil
}

func creditNoteEInvoiceFilename(number string) string {
	return "einvoice-credit-note-" + strings.NewReplacer("/", "-", " ", "-").Replace(number) + ".json"
}

// CreditNoteEInvoice builds, validates and renders the e-invoice JSON of one
// credit note: a CRN document that refers to the invoice it answers, with the
// credit note's own number, date and (positive) amounts. Only credit notes of
// B2B and export invoices have one, like the invoices themselves.
func (s *Service) CreditNoteEInvoice(ctx context.Context, creditNoteID string) (EInvoiceResult, error) {
	var cn store.CreditNote
	var inv store.Invoice
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		if cn, err = store.GetCreditNote(ctx, q, creditNoteID); err != nil {
			return err
		}
		inv, err = store.GetInvoice(ctx, q, cn.InvoiceID)
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return EInvoiceResult{}, invalid("no credit note %q", creditNoteID)
	}
	if err != nil {
		return EInvoiceResult{}, err
	}
	if !einvoice.Eligible(inv.SupplyType) {
		return EInvoiceResult{}, invalid("credit note %s is against a %s invoice: only B2B and export documents have an e-invoice", cn.Number, inv.SupplyType)
	}
	src, err := s.documentSource(ctx, inv, cn.Number, cn.IssuedAt, cn.Lines, amountsOf(cn.Taxable, cn.CGST, cn.SGST, cn.IGST, cn.Total))
	if err != nil {
		return EInvoiceResult{}, err
	}
	src.Credit = &einvoice.CreditRef{InvoiceNumber: inv.Number, InvoiceDate: inv.IssuedAt}
	doc, err := einvoice.Build(src)
	if err != nil {
		return EInvoiceResult{}, err
	}
	raw, err := einvoice.Marshal(doc)
	if err != nil {
		return EInvoiceResult{}, err
	}
	if err := einvoice.Validate(raw); err != nil {
		return EInvoiceResult{}, &einvoice.Problems{List: []string{err.Error()}}
	}
	return EInvoiceResult{Invoice: inv, Doc: doc, JSON: raw, Filename: creditNoteEInvoiceFilename(cn.Number)}, nil
}

// EInvoiceRow is one invoice of a month export.
type EInvoiceRow struct {
	Invoice store.Invoice
	Buyer   string
	// Status is ready (can be uploaded), done (the portal's answer is recorded)
	// or problem (cannot be built; see Problems).
	Status   string
	Problems []string
}

// EInvoiceExport is a month of e-invoices of one tax entity.
type EInvoiceExport struct {
	Entity   store.TaxEntity
	Month    string
	Rows     []EInvoiceRow
	Ready    int
	Done     int
	Problems int
	// SkippedB2C counts invoices that need no e-invoice (consumers).
	SkippedB2C int
	// JSON is the array the portal's bulk upload takes: the ready invoices, and
	// the done ones too when includeDone was set.
	JSON     []byte
	Filename string
}

// EInvoiceMonth lists the month's invoices of a tax entity with their e-invoice
// status and builds the bulk upload file.
func (s *Service) EInvoiceMonth(ctx context.Context, entityID, month string, includeDone bool) (EInvoiceExport, error) {
	from, to, err := monthRange(month)
	if err != nil {
		return EInvoiceExport{}, err
	}
	var ent store.TaxEntity
	var invs []store.Invoice
	err = s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if ent, err = store.GetTaxEntity(ctx, q, entityID); err != nil {
			return err
		}
		invs, err = store.InvoicesInRange(ctx, q, entityID, from, to)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return EInvoiceExport{}, invalid("no tax entity %q", entityID)
	}
	if err != nil {
		return EInvoiceExport{}, err
	}
	out := EInvoiceExport{Entity: ent, Month: month, Filename: fmt.Sprintf("einvoice-%s-%s.json", entityID, month)}
	var docs []*einvoice.Invoice
	for _, inv := range invs {
		if !einvoice.Eligible(inv.SupplyType) {
			out.SkippedB2C++
			continue
		}
		var rcp Recipient
		_ = json.Unmarshal(inv.Recipient, &rcp)
		row := EInvoiceRow{Invoice: inv, Buyer: rcp.Name}
		doc, err := s.buildEInvoice(ctx, inv)
		var pe *einvoice.Problems
		switch {
		case errors.As(err, &pe):
			row.Status, row.Problems = "problem", pe.List
		case err != nil:
			row.Status, row.Problems = "problem", []string{err.Error()}
		case inv.IRN != "":
			row.Status = "done"
		default:
			row.Status = "ready"
		}
		switch {
		case row.Status == "problem" && inv.IRN != "":
			out.Done++ // already registered; a problem in rebuilding it does not matter now
			row.Status, row.Problems = "done", nil
		case row.Status == "problem":
			out.Problems++
		case row.Status == "done":
			out.Done++
			if includeDone {
				docs = append(docs, doc)
			}
		default:
			out.Ready++
			docs = append(docs, doc)
		}
		out.Rows = append(out.Rows, row)
	}
	if out.JSON, err = einvoice.MarshalBatch(docs); err != nil {
		return EInvoiceExport{}, err
	}
	if err := einvoice.ValidateBatch(out.JSON); err != nil {
		return EInvoiceExport{}, fmt.Errorf("invoice: a built e-invoice does not validate (a bug): %w", err)
	}
	return out, nil
}

// EInvoiceRecord is what the operator pastes back from the portal.
type EInvoiceRecord struct {
	IRN      string
	AckNo    string
	AckDate  string // "2026-10-18 10:15:30" as the portal prints it (IST)
	SignedQR string
}

var (
	irnRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ackNoRE = regexp.MustCompile(`^[0-9]{1,15}$`)
	b64RE   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

const maxSignedQR = 4000

var ackLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "02/01/2006 15:04:05", "02-01-2006 15:04:05", time.RFC3339}

func parseAckDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range ackLayouts {
		if t, err := time.ParseInLocation(l, s, tax.IST); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// verifyPortalSignature checks the RS256 signature of the signed QR code
// against the portal keys of the einvoice_irp_keys setting. With no key
// configured it verifies nothing and says so (false); with keys, a token that is
// not signed by one of them is refused.
func (s *Service) verifyPortalSignature(ctx context.Context, q store.Queryer, orgID, token string) (bool, error) {
	var pems []string
	if _, err := s.orgValue(ctx, q, orgID, "einvoice_irp_keys", &pems); err != nil {
		return false, err
	}
	if len(pems) == 0 {
		return false, nil
	}
	keys, err := irp.ParseKeys(pems)
	if err != nil {
		return false, fmt.Errorf("invoice: the einvoice_irp_keys setting: %w", err)
	}
	if _, err := irp.Verify(token, keys); err != nil {
		return false, invalid("the signed QR code's signature is not valid for the portal keys you configured (%s)", strings.TrimPrefix(err.Error(), "irp: "))
	}
	return true, nil
}

// checkSignedQR makes sure the pasted token is the portal's signed QR code of
// this very invoice: a JWT-shaped string whose claims name the same IRN,
// document number and seller. The signature is verified separately, against the
// configured portal keys (verifyPortalSignature).
func checkSignedQR(qr, irn, number, sellerGSTIN string) error {
	parts := strings.Split(qr, ".")
	if len(parts) != 3 || !b64RE.MatchString(parts[0]) || !b64RE.MatchString(parts[1]) || !b64RE.MatchString(parts[2]) {
		return invalid("the signed QR code must be the long text of three parts separated by dots that the portal returns as SignedQRCode")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return invalid("the signed QR code is not readable (its middle part is not base64)")
	}
	var claims struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || len(claims.Data) == 0 {
		return invalid("the signed QR code has no invoice data in it")
	}
	data := claims.Data
	var asText string
	if json.Unmarshal(data, &asText) == nil { // the portal wraps the data as a JSON string
		data = json.RawMessage(asText)
	}
	var d struct {
		SellerGstin string `json:"SellerGstin"`
		DocNo       string `json:"DocNo"`
		Irn         string `json:"Irn"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return invalid("the signed QR code's invoice data is not readable")
	}
	switch {
	case !strings.EqualFold(d.Irn, irn):
		return invalid("the signed QR code is for a different IRN than the one entered")
	case d.DocNo != number:
		return invalid("the signed QR code is for document %q, not for this invoice %s", d.DocNo, number)
	case d.SellerGstin != sellerGSTIN:
		return invalid("the signed QR code is for seller %s, not this invoice's seller %s", d.SellerGstin, sellerGSTIN)
	}
	return nil
}

// RecordEInvoice stores the portal's answer for an invoice: IRN, acknowledgement
// number and date, and the signed QR code. The invoice's PDF is rendered again
// so that it carries them. Recording again replaces the earlier values.
func (s *Service) RecordEInvoice(ctx context.Context, actor audit.Actor, invoiceID string, in EInvoiceRecord) (store.Invoice, error) {
	in.IRN = strings.ToLower(strings.TrimSpace(in.IRN))
	in.AckNo = strings.TrimSpace(in.AckNo)
	in.SignedQR = strings.TrimSpace(in.SignedQR)
	switch {
	case !irnRE.MatchString(in.IRN):
		return store.Invoice{}, invalid("the IRN is 64 hexadecimal characters (0-9, a-f)")
	case !ackNoRE.MatchString(in.AckNo):
		return store.Invoice{}, invalid("the acknowledgement number is up to 15 digits")
	case len(in.SignedQR) == 0 || len(in.SignedQR) > maxSignedQR:
		return store.Invoice{}, invalid("the signed QR code is missing or longer than %d characters", maxSignedQR)
	}
	ackDate, ok := parseAckDate(in.AckDate)
	if !ok {
		return store.Invoice{}, invalid("the acknowledgement date should look like 2026-10-18 10:15:30 (IST)")
	}
	now := s.now().UTC()
	var out store.Invoice
	err := s.db.Write(ctx, func(tx store.Tx) error {
		inv, err := store.GetInvoice(ctx, tx, invoiceID)
		if errors.Is(err, store.ErrNotFound) {
			return invalid("no invoice %q", invoiceID)
		}
		if err != nil {
			return err
		}
		if !einvoice.Eligible(inv.SupplyType) {
			return invalid("invoice %s is %s: only B2B and export invoices have an e-invoice", inv.Number, inv.SupplyType)
		}
		if ackDate.After(now.Add(24*time.Hour)) || ackDate.Before(inv.IssuedAt.Add(-24*time.Hour)) {
			return invalid("the acknowledgement date %s is not plausible for an invoice issued on %s", in.AckDate, inv.IssuedAt.In(tax.IST).Format("2006-01-02"))
		}
		var sup Supplier
		if err := json.Unmarshal(inv.Supplier, &sup); err != nil {
			return err
		}
		if err := checkSignedQR(in.SignedQR, in.IRN, inv.Number, sup.GSTIN); err != nil {
			return err
		}
		verified, err := s.verifyPortalSignature(ctx, tx, inv.OrgID, in.SignedQR)
		if err != nil {
			return err
		}
		if err := store.SetInvoiceEInvoice(ctx, tx, inv.ID, in.IRN, in.AckNo, ackDate, in.SignedQR, now); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return invalid("this IRN is already recorded on another invoice")
			}
			return err
		}
		// The PDF is made again with the IRN and the QR code; the hosted link is the same.
		if err := store.ClearInvoicePDF(ctx, tx, inv.ID); err != nil {
			return err
		}
		if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobRender, Payload: map[string]string{"kind": kindInvoice, "id": inv.ID},
			DedupeKey: JobRender + ":einvoice:" + inv.ID + ":" + in.IRN[:12] + ":" + in.AckNo}); err != nil {
			return err
		}
		if out, err = store.GetInvoice(ctx, tx, inv.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: inv.OrgID, Actor: actor, Action: "invoice.einvoice_record", Target: inv.ID,
			Before: map[string]string{"irn": inv.IRN, "ack_no": inv.AckNo},
			After: map[string]string{"irn": in.IRN, "ack_no": in.AckNo, "ack_date": ackDate.In(tax.IST).Format("2006-01-02 15:04:05"),
				"signature_verified": fmt.Sprint(verified)}})
	})
	return out, err
}
