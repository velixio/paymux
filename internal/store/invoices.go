package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Invoice is a row of invoices. Supplier, Recipient and Lines are JSON
// snapshots; amounts are minor units of Currency.
type Invoice struct {
	ID, OrgID, Mode, AppID, TaxEntityID, PaymentID, AttemptID, SubscriptionID string
	Number, SeriesPrefix, FiscalYear                                          string
	IssuedAt                                                                  time.Time
	SupplyType, PlaceOfSupply                                                 string
	InterState                                                                bool
	Currency                                                                  string
	Supplier, Recipient, Lines                                                json.RawMessage
	Taxable, CGST, SGST, IGST, Total                                          int64
	GSTRateBP                                                                 int
	FXRate, FXSource                                                          string
	Flags                                                                     []string
	PaymentMethod, PaymentRef, LUTNumber                                      string
	TemplateVersion, PDFBlobID, AccessTokenHash                               string
}

// HasFlag reports whether the invoice carries flag f.
func (i Invoice) HasFlag(f string) bool { return hasFlag(i.Flags, f) }

func hasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

const invoiceCols = `id, org_id, mode, app_id, tax_entity_id, payment_id, attempt_id, COALESCE(subscription_id,''), number,
	series_prefix, fiscal_year, issued_at, supply_type, place_of_supply, inter_state, currency, supplier, recipient, lines,
	taxable, cgst, sgst, igst, total, gst_rate_bp, COALESCE(fx_rate_inr,''), COALESCE(fx_source,''), flags, payment_method,
	payment_ref, COALESCE(lut_number,''), template_version, COALESCE(pdf_blob_id,''), access_token_hash`

func scanInvoice(row interface{ Scan(...any) error }) (Invoice, error) {
	var i Invoice
	var issued int64
	var inter int
	var sup, rcp, lines, flags string
	err := row.Scan(&i.ID, &i.OrgID, &i.Mode, &i.AppID, &i.TaxEntityID, &i.PaymentID, &i.AttemptID, &i.SubscriptionID,
		&i.Number, &i.SeriesPrefix, &i.FiscalYear, &issued, &i.SupplyType, &i.PlaceOfSupply, &inter, &i.Currency,
		&sup, &rcp, &lines, &i.Taxable, &i.CGST, &i.SGST, &i.IGST, &i.Total, &i.GSTRateBP, &i.FXRate, &i.FXSource, &flags,
		&i.PaymentMethod, &i.PaymentRef, &i.LUTNumber, &i.TemplateVersion, &i.PDFBlobID, &i.AccessTokenHash)
	if err != nil {
		return Invoice{}, notFound(err)
	}
	i.IssuedAt, i.InterState = fromMS(issued), inter == 1
	i.Supplier, i.Recipient, i.Lines = json.RawMessage(sup), json.RawMessage(rcp), json.RawMessage(lines)
	if err := json.Unmarshal([]byte(flags), &i.Flags); err != nil {
		return Invoice{}, fmt.Errorf("invoice %s flags: %w", i.ID, err)
	}
	return i, nil
}

// InsertInvoice inserts i.
func InsertInvoice(ctx context.Context, tx Tx, i Invoice) error {
	flags, _ := json.Marshal(nonNilSlice(i.Flags))
	_, err := tx.ExecContext(ctx, `INSERT INTO invoices (id, org_id, mode, app_id, tax_entity_id, payment_id, attempt_id,
		subscription_id, number, series_prefix, fiscal_year, issued_at, supply_type, place_of_supply, inter_state, currency,
		supplier, recipient, lines, taxable, cgst, sgst, igst, total, gst_rate_bp, fx_rate_inr, fx_source, flags,
		payment_method, payment_ref, lut_number, template_version, pdf_blob_id, access_token_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.OrgID, i.Mode, i.AppID, i.TaxEntityID, i.PaymentID, i.AttemptID, nullIfEmpty(i.SubscriptionID), i.Number,
		i.SeriesPrefix, i.FiscalYear, ms(i.IssuedAt), i.SupplyType, i.PlaceOfSupply, boolInt(i.InterState), i.Currency,
		string(i.Supplier), string(i.Recipient), string(i.Lines), i.Taxable, i.CGST, i.SGST, i.IGST, i.Total, i.GSTRateBP,
		nullIfEmpty(i.FXRate), nullIfEmpty(i.FXSource), string(flags), i.PaymentMethod, i.PaymentRef, nullIfEmpty(i.LUTNumber),
		i.TemplateVersion, nullIfEmpty(i.PDFBlobID), i.AccessTokenHash)
	return err
}

// GetInvoice loads an invoice by id.
func GetInvoice(ctx context.Context, q Queryer, id string) (Invoice, error) {
	return scanInvoice(q.QueryRowContext(ctx, "SELECT "+invoiceCols+" FROM invoices WHERE id = ?", id))
}

func listInvoices(ctx context.Context, q Queryer, where string, args ...any) ([]Invoice, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+invoiceCols+" FROM invoices WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		i, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// InvoicesForPayment lists a payment's invoices, oldest first.
func InvoicesForPayment(ctx context.Context, q Queryer, paymentID string) ([]Invoice, error) {
	return listInvoices(ctx, q, "payment_id = ? ORDER BY issued_at, id", paymentID)
}

// InvoicesInRange lists a tax entity's invoices issued in [from, to) ordered
// by series then number.
func InvoicesInRange(ctx context.Context, q Queryer, entityID string, from, to time.Time) ([]Invoice, error) {
	return listInvoices(ctx, q, "tax_entity_id = ? AND issued_at >= ? AND issued_at < ? ORDER BY series_prefix, number",
		entityID, ms(from), ms(to))
}

// InvoicesPendingFX lists invoices flagged fx_pending for a currency.
func InvoicesPendingFX(ctx context.Context, q Queryer, orgID, currency string) ([]Invoice, error) {
	return listInvoices(ctx, q, "org_id = ? AND currency = ? AND fx_rate_inr IS NULL AND flags LIKE '%fx_pending%' ORDER BY issued_at",
		orgID, currency)
}

// SetInvoiceFX records the FX rate of an invoice whose rate was pending and
// replaces its flags.
func SetInvoiceFX(ctx context.Context, tx Tx, id, rate, source string, flags []string) error {
	fl, _ := json.Marshal(nonNilSlice(flags))
	_, err := tx.ExecContext(ctx, "UPDATE invoices SET fx_rate_inr = ?, fx_source = ?, flags = ? WHERE id = ?", rate, source, string(fl), id)
	return err
}

// SetInvoicePDF records the rendered PDF blob of an invoice.
func SetInvoicePDF(ctx context.Context, tx Tx, id, blobID string) error {
	_, err := tx.ExecContext(ctx, "UPDATE invoices SET pdf_blob_id = ? WHERE id = ?", blobID, id)
	return err
}

// CreditNote is a row of credit_notes.
type CreditNote struct {
	ID, OrgID, Mode, AppID, TaxEntityID, InvoiceID, PaymentID, RefundID, DisputeID string
	Number, SeriesPrefix, FiscalYear                                               string
	IssuedAt                                                                       time.Time
	Reason, Currency                                                               string
	Lines                                                                          json.RawMessage
	Taxable, CGST, SGST, IGST, Total                                               int64
	Flags                                                                          []string
	TemplateVersion, PDFBlobID, AccessTokenHash                                    string
}

const creditNoteCols = `id, org_id, mode, app_id, tax_entity_id, invoice_id, payment_id, COALESCE(refund_id,''),
	COALESCE(dispute_id,''), number, series_prefix, fiscal_year, issued_at, reason, currency, lines, taxable, cgst, sgst,
	igst, total, flags, template_version, COALESCE(pdf_blob_id,''), access_token_hash`

func scanCreditNote(row interface{ Scan(...any) error }) (CreditNote, error) {
	var c CreditNote
	var issued int64
	var lines, flags string
	err := row.Scan(&c.ID, &c.OrgID, &c.Mode, &c.AppID, &c.TaxEntityID, &c.InvoiceID, &c.PaymentID, &c.RefundID, &c.DisputeID,
		&c.Number, &c.SeriesPrefix, &c.FiscalYear, &issued, &c.Reason, &c.Currency, &lines, &c.Taxable, &c.CGST, &c.SGST,
		&c.IGST, &c.Total, &flags, &c.TemplateVersion, &c.PDFBlobID, &c.AccessTokenHash)
	if err != nil {
		return CreditNote{}, notFound(err)
	}
	c.IssuedAt, c.Lines = fromMS(issued), json.RawMessage(lines)
	if err := json.Unmarshal([]byte(flags), &c.Flags); err != nil {
		return CreditNote{}, fmt.Errorf("credit note %s flags: %w", c.ID, err)
	}
	return c, nil
}

// InsertCreditNote inserts c. A second credit note for the same refund fails
// with ErrConflict.
func InsertCreditNote(ctx context.Context, tx Tx, c CreditNote) error {
	flags, _ := json.Marshal(nonNilSlice(c.Flags))
	_, err := tx.ExecContext(ctx, `INSERT INTO credit_notes (id, org_id, mode, app_id, tax_entity_id, invoice_id, payment_id,
		refund_id, dispute_id, number, series_prefix, fiscal_year, issued_at, reason, currency, lines, taxable, cgst, sgst,
		igst, total, flags, template_version, pdf_blob_id, access_token_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.OrgID, c.Mode, c.AppID, c.TaxEntityID, c.InvoiceID, c.PaymentID, nullIfEmpty(c.RefundID),
		nullIfEmpty(c.DisputeID), c.Number, c.SeriesPrefix, c.FiscalYear, ms(c.IssuedAt), c.Reason, c.Currency,
		string(c.Lines), c.Taxable, c.CGST, c.SGST, c.IGST, c.Total, string(flags), c.TemplateVersion,
		nullIfEmpty(c.PDFBlobID), c.AccessTokenHash)
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: credit note already exists", ErrConflict)
	}
	return err
}

// GetCreditNote loads a credit note by id.
func GetCreditNote(ctx context.Context, q Queryer, id string) (CreditNote, error) {
	return scanCreditNote(q.QueryRowContext(ctx, "SELECT "+creditNoteCols+" FROM credit_notes WHERE id = ?", id))
}

func listCreditNotes(ctx context.Context, q Queryer, where string, args ...any) ([]CreditNote, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+creditNoteCols+" FROM credit_notes WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditNote
	for rows.Next() {
		c, err := scanCreditNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreditNotesForInvoice lists an invoice's credit notes, oldest first.
func CreditNotesForInvoice(ctx context.Context, q Queryer, invoiceID string) ([]CreditNote, error) {
	return listCreditNotes(ctx, q, "invoice_id = ? ORDER BY issued_at, id", invoiceID)
}

// CreditNotesForPayment lists a payment's credit notes, oldest first.
func CreditNotesForPayment(ctx context.Context, q Queryer, paymentID string) ([]CreditNote, error) {
	return listCreditNotes(ctx, q, "payment_id = ? ORDER BY issued_at, number, id", paymentID)
}

// CreditNotesInRange lists a tax entity's credit notes issued in [from, to).
func CreditNotesInRange(ctx context.Context, q Queryer, entityID string, from, to time.Time) ([]CreditNote, error) {
	return listCreditNotes(ctx, q, "tax_entity_id = ? AND issued_at >= ? AND issued_at < ? ORDER BY series_prefix, number",
		entityID, ms(from), ms(to))
}

// SetCreditNotePDF records the rendered PDF blob of a credit note.
func SetCreditNotePDF(ctx context.Context, tx Tx, id, blobID string) error {
	_, err := tx.ExecContext(ctx, "UPDATE credit_notes SET pdf_blob_id = ? WHERE id = ?", blobID, id)
	return err
}

// RefundFlag is a tax flag on a refund.
type RefundFlag struct {
	RefundID, Flag string
	CreatedAt      time.Time
}

// AddRefundFlag records a tax flag on a refund; repeating it is a no-op.
func AddRefundFlag(ctx context.Context, tx Tx, refundID, flag string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO refund_flags (refund_id, flag, created_at) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, refundID, flag, ms(now))
	return err
}

// RefundFlagsInRange lists refund flags raised in [from, to) for refunds of
// the apps that issue invoices under a tax entity.
func RefundFlagsInRange(ctx context.Context, q Queryer, entityID string, from, to time.Time) ([]RefundFlag, error) {
	rows, err := q.QueryContext(ctx, `SELECT f.refund_id, f.flag, f.created_at FROM refund_flags f
		JOIN refunds r ON r.id = f.refund_id JOIN app_tax_profiles t ON t.app_id = r.app_id
		WHERE t.tax_entity_id = ? AND f.created_at >= ? AND f.created_at < ? ORDER BY f.created_at, f.refund_id`,
		entityID, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefundFlag
	for rows.Next() {
		var f RefundFlag
		var at int64
		if err := rows.Scan(&f.RefundID, &f.Flag, &at); err != nil {
			return nil, err
		}
		f.CreatedAt = fromMS(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// RefundFlags lists the flags of one refund.
func RefundFlags(ctx context.Context, q Queryer, refundID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT flag FROM refund_flags WHERE refund_id = ? ORDER BY flag", refundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DocumentSend records one email delivery attempt of a document.
type DocumentSend struct {
	ID, Kind, DocID, Recipient, Status, Error string
	SentAt                                    time.Time
}

// InsertDocumentSend records a send.
func InsertDocumentSend(ctx context.Context, tx Tx, s DocumentSend) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO document_sends (id, doc_kind, doc_id, recipient, status, error, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, s.ID, s.Kind, s.DocID, s.Recipient, s.Status, nullIfEmpty(s.Error), ms(s.SentAt))
	return err
}

// DocumentSends lists the sends of a document, oldest first.
func DocumentSends(ctx context.Context, q Queryer, kind, docID string) ([]DocumentSend, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, doc_kind, doc_id, recipient, status, COALESCE(error,''), sent_at
		FROM document_sends WHERE doc_kind = ? AND doc_id = ? ORDER BY sent_at, id`, kind, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocumentSend
	for rows.Next() {
		var s DocumentSend
		var at int64
		if err := rows.Scan(&s.ID, &s.Kind, &s.DocID, &s.Recipient, &s.Status, &s.Error, &at); err != nil {
			return nil, err
		}
		s.SentAt = fromMS(at)
		out = append(out, s)
	}
	return out, rows.Err()
}
