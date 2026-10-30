// Package render turns a plain Document into a GST tax invoice or credit note
// PDF (spec §10 "Content"). It has no database or HTTP knowledge: the caller
// fills the Document from an issued invoice, and renders again from the
// stored template version when a document is regenerated.
package render

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/tax"
)

// Version1 is the first layout. Documents store the version that rendered
// them; old versions must keep rendering identically.
const Version1 = "v1"

// Kind is the document type.
type Kind string

const (
	KindInvoice    Kind = "invoice"
	KindCreditNote Kind = "credit_note"
)

// Party is the supplier or the recipient.
type Party struct {
	Name        string // recipient name
	LegalName   string // supplier legal name
	TradeName   string // supplier trade name
	Address     string
	GSTIN       string // optional for recipients
	StateName   string
	StateCode   string // 2-digit
	Country     string // ISO alpha-2 for recipients ("IN" in India)
	CountryName string // shown for foreign recipients
	Logo        []byte // supplier logo, PNG, optional
}

// Line is one invoice line. Rates are basis points (1800 = 18%); amounts are
// paise.
type Line struct {
	Description string
	SAC         string
	Qty         int64
	Unit        string // "service"
	Taxable     int64
	CGSTRateBP  int
	SGSTRateBP  int
	IGSTRateBP  int
	CGST        int64
	SGST        int64
	IGST        int64
}

// Document is everything the template prints.
type Document struct {
	Kind      Kind
	Number    string
	IssueDate time.Time // printed in IST

	Supplier  Party
	Recipient Party

	PlaceOfSupply string // e.g. "Karnataka (29)" or "Outside India (United States)"
	SupplyType    tax.SupplyType
	InterState    bool // IGST layout (also true for exports)

	Lines  []Line
	Totals tax.Amounts // paise; Total must equal the sum of the lines

	PaymentMethod string
	PaymentRef    string // provider reference

	// Credit notes.
	OriginalNumber string
	OriginalDate   time.Time
	Reason         string

	// Exports under LUT.
	ExportEndorsement string
	LUTNumber         string

	// Foreign-currency payments: the amount paid and the INR rate used,
	// e.g. Currency "USD", ForeignAmount 1200 (12.00), FXRate "83.2500".
	Currency      string
	ForeignAmount int64
	FXRate        string

	SignatoryName string
	// SignedBy, when set, says the PDF is going to be digitally signed by this
	// name (the Signer of Options does it); the page then reads "Digitally
	// signed by ..." in place of "This is a computer-generated document".
	SignedBy        string
	FooterNote      string
	TemplateVersion string

	// EInvoice is what the Invoice Registration Portal returned for the
	// invoice; nil until the operator has recorded it. Credit notes ignore it.
	EInvoice *EInvoiceInfo
}

// EInvoiceInfo is the portal's answer for one invoice.
type EInvoiceInfo struct {
	IRN     string
	AckNo   string
	AckDate time.Time // printed in IST
	// QR is the signed QR code token; it is drawn as a QR code, or left out
	// when it is too long for one.
	QR string
}

// Signer signs a finished PDF (PAdES with the tax entity's DSC, see
// internal/invoice/pades).
type Signer interface {
	Sign(pdf []byte) ([]byte, error)
}

// Options tunes rendering.
type Options struct {
	Signer Signer // optional; nil leaves the signatory-note legend only
}

func (d Document) validate() error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if d.Kind != KindInvoice && d.Kind != KindCreditNote {
		add("kind %q must be invoice or credit_note", d.Kind)
	}
	if strings.TrimSpace(d.Number) == "" {
		add("number is required")
	}
	if len(d.Number) > 16 {
		add("number %q exceeds 16 characters (CGST Rule 46)", d.Number)
	}
	if d.IssueDate.IsZero() {
		add("issue date is required")
	}
	if len(d.Lines) == 0 {
		add("at least one line is required")
	}
	if d.Kind == KindCreditNote && (d.OriginalNumber == "" || d.OriginalDate.IsZero()) {
		add("credit note needs the original invoice number and date")
	}
	if d.SupplyType == tax.ExportLUT && d.LUTNumber == "" {
		add("export under LUT needs the LUT number")
	}
	if d.TemplateVersion != Version1 {
		add("unknown template version %q", d.TemplateVersion)
	}
	var sum tax.Amounts
	for _, l := range d.Lines {
		sum = sum.Add(tax.Amounts{Taxable: l.Taxable, CGST: l.CGST, SGST: l.SGST, IGST: l.IGST,
			Total: l.Taxable + l.CGST + l.SGST + l.IGST})
	}
	if len(d.Lines) > 0 && sum != d.Totals {
		add("totals %+v do not equal the sum of the lines %+v", d.Totals, sum)
	}
	return errors.Join(errs...)
}
