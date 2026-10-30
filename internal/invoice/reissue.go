package invoice

import (
	"context"
	"errors"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
)

// ReissueResult is what ReissueInvoice created.
type ReissueResult struct {
	CreditNote store.CreditNote
	Invoice    store.Invoice
}

// ReissueInvoice corrects an invoice issued with wrong billing details: it
// issues a full credit note against it and a new invoice for the same payment,
// classified from the corrected details, which are also saved on the customer.
// Everything happens in one transaction, so a refusal leaves no trace.
//
// It refuses (ErrInvalid) when the invoice already has credit notes (partial
// refunds make "full credit" ambiguous), when the statutory credit-note cutoff
// has passed, or when the corrected details do not classify (bad GSTIN or State).
func (s *Service) ReissueInvoice(ctx context.Context, actor audit.Actor, invoiceID string, billing store.Billing, reason string) (ReissueResult, error) {
	var res ReissueResult
	err := s.db.Write(ctx, func(tx store.Tx) error {
		inv, err := store.GetInvoice(ctx, tx, invoiceID)
		if errors.Is(err, store.ErrNotFound) {
			return invalid("no invoice %q", invoiceID)
		}
		if err != nil {
			return err
		}
		prior, err := store.CreditNotesForInvoice(ctx, tx, inv.ID)
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			return invalid("invoice %s already has credit notes and cannot be reissued", inv.Number)
		}
		now := s.now().UTC()
		deadline, hasCutoff, err := s.creditNoteDeadline(ctx, tx, inv.OrgID, inv.IssuedAt)
		if err != nil {
			return err
		}
		if hasCutoff && now.After(deadline) {
			return invalid("the credit note cutoff for invoice %s has passed", inv.Number)
		}
		p, err := store.GetPayment(ctx, tx, inv.PaymentID)
		if err != nil {
			return err
		}
		a, err := store.GetAttempt(ctx, tx, inv.AttemptID)
		if err != nil {
			return err
		}
		if p.CustomerID == "" {
			return invalid("the payment of invoice %s has no customer record to correct", inv.Number)
		}
		c, err := store.GetCustomer(ctx, tx, p.CustomerID)
		if err != nil {
			return err
		}
		ent, err := store.GetTaxEntity(ctx, tx, inv.TaxEntityID)
		if err != nil {
			return err
		}
		prof, err := store.GetAppTaxProfile(ctx, tx, inv.AppID)
		if err != nil {
			return err
		}

		if billing.Country == "" {
			billing.Country = "IN"
		}
		sup := tax.Supplier{GSTIN: ent.GSTIN, StateCode: ent.StateCode}
		if ent.LUTNumber != "" {
			sup.LUT = &tax.LUT{Number: ent.LUTNumber, ValidFrom: ent.LUTValidFrom, ValidTo: ent.LUTValidTo}
		}
		if _, err := tax.Classify(sup, tax.Recipient{GSTIN: billing.GSTIN, StateCode: billing.StateCode, Country: billing.Country}, now, inv.Currency != "INR"); err != nil {
			return invalid("billing details: %v", err)
		}
		if err := store.UpdateCustomerBilling(ctx, tx, c.ID, billing, now); err != nil {
			return err
		}
		c.Billing = billing

		cn, err := s.insertCreditNote(ctx, tx, inv, creditRefs{PaymentID: inv.PaymentID}, amountsOf(inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total),
			firstNonEmpty(reason, "Reissued with corrected billing details"), now)
		if err != nil {
			return err
		}
		ni, err := s.issue(ctx, tx, p, a, c, prof, []string{FlagReissued})
		if err != nil {
			return err
		}
		res = ReissueResult{CreditNote: *cn, Invoice: *ni}
		return audit.Record(ctx, tx, audit.Entry{OrgID: inv.OrgID, Actor: actor, Action: "invoice.reissue", Target: inv.ID,
			After: map[string]string{"credit_note_id": cn.ID, "credit_note_number": cn.Number, "invoice_id": ni.ID, "invoice_number": ni.Number,
				"reason": reason}})
	})
	return res, err
}
