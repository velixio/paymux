package invoice

import (
	"encoding/json"
	"time"

	"github.com/paymux/paymux/internal/store"
)

// InvoiceView is the public JSON shape of an invoice (API and events). The
// access token hash is never included.
type InvoiceView struct {
	ID              string          `json:"id"`
	Object          string          `json:"object"`
	Number          string          `json:"number"`
	PaymentID       string          `json:"payment_id"`
	SubscriptionID  string          `json:"subscription_id,omitempty"`
	IssuedAt        time.Time       `json:"issued_at"`
	SupplyType      string          `json:"supply_type"`
	PlaceOfSupply   string          `json:"place_of_supply"`
	Currency        string          `json:"currency"`
	Supplier        json.RawMessage `json:"supplier"`
	Recipient       json.RawMessage `json:"recipient"`
	Lines           json.RawMessage `json:"lines"`
	Taxable         int64           `json:"taxable"`
	CGST            int64           `json:"cgst"`
	SGST            int64           `json:"sgst"`
	IGST            int64           `json:"igst"`
	Total           int64           `json:"total"`
	FXRateINR       string          `json:"fx_rate_inr,omitempty"`
	Flags           []string        `json:"flags"`
	TemplateVersion string          `json:"template_version"`
	PDFReady        bool            `json:"pdf_ready"`
	// E-invoice (GST IRP) registration, once the operator has recorded it.
	IRN     string     `json:"irn,omitempty"`
	AckNo   string     `json:"ack_no,omitempty"`
	AckDate *time.Time `json:"ack_date,omitempty"`
}

// ViewInvoice renders i.
func ViewInvoice(i store.Invoice) InvoiceView {
	flags := i.Flags
	if flags == nil {
		flags = []string{}
	}
	return InvoiceView{ID: i.ID, Object: "invoice", Number: i.Number, PaymentID: i.PaymentID, SubscriptionID: i.SubscriptionID,
		IssuedAt: i.IssuedAt, SupplyType: i.SupplyType, PlaceOfSupply: i.PlaceOfSupply, Currency: i.Currency,
		Supplier: i.Supplier, Recipient: i.Recipient, Lines: i.Lines, Taxable: i.Taxable, CGST: i.CGST, SGST: i.SGST,
		IGST: i.IGST, Total: i.Total, FXRateINR: i.FXRate, Flags: flags, TemplateVersion: i.TemplateVersion,
		PDFReady: i.PDFBlobID != "", IRN: i.IRN, AckNo: i.AckNo, AckDate: ackDate(i)}
}

func ackDate(i store.Invoice) *time.Time {
	if i.AckDate.IsZero() {
		return nil
	}
	t := i.AckDate.UTC()
	return &t
}

// CreditNoteView is the public JSON shape of a credit note.
type CreditNoteView struct {
	ID              string          `json:"id"`
	Object          string          `json:"object"`
	Number          string          `json:"number"`
	InvoiceID       string          `json:"invoice_id"`
	PaymentID       string          `json:"payment_id"`
	RefundID        string          `json:"refund_id,omitempty"`
	DisputeID       string          `json:"dispute_id,omitempty"`
	IssuedAt        time.Time       `json:"issued_at"`
	Reason          string          `json:"reason"`
	Currency        string          `json:"currency"`
	Lines           json.RawMessage `json:"lines"`
	Taxable         int64           `json:"taxable"`
	CGST            int64           `json:"cgst"`
	SGST            int64           `json:"sgst"`
	IGST            int64           `json:"igst"`
	Total           int64           `json:"total"`
	Flags           []string        `json:"flags"`
	TemplateVersion string          `json:"template_version"`
	PDFReady        bool            `json:"pdf_ready"`
}

// ViewCreditNote renders c.
func ViewCreditNote(c store.CreditNote) CreditNoteView {
	flags := c.Flags
	if flags == nil {
		flags = []string{}
	}
	return CreditNoteView{ID: c.ID, Object: "credit_note", Number: c.Number, InvoiceID: c.InvoiceID, PaymentID: c.PaymentID,
		RefundID: c.RefundID, DisputeID: c.DisputeID, IssuedAt: c.IssuedAt, Reason: c.Reason, Currency: c.Currency,
		Lines: c.Lines, Taxable: c.Taxable, CGST: c.CGST, SGST: c.SGST, IGST: c.IGST, Total: c.Total, Flags: flags,
		TemplateVersion: c.TemplateVersion, PDFReady: c.PDFBlobID != ""}
}
