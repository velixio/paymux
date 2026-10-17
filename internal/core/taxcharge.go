package core

import (
	"context"
	"errors"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// chargeAmount is what the buyer is asked to pay for a price of `amount`.
//
// When the app issues invoices and its tax profile says prices exclude tax
// (prices_include_tax=false), `amount` is the taxable value and the buyer pays
// taxable + GST, so the invoice total equals the amount received. In every
// other case the amount is charged as given.
//
// The GST is computed the way the invoice will classify the sale, from the
// buyer details known now: a foreign buyer paying in a foreign currency under
// a valid LUT is zero-rated, everything else carries the app's rate. Which
// head carries it (CGST+SGST or IGST) does not change the total.
func (s *Service) chargeAmount(ctx context.Context, q store.Queryer, appID, currency string, amount int64, buyer store.Billing) (int64, error) {
	prof, err := store.GetAppTaxProfile(ctx, q, appID)
	if errors.Is(err, store.ErrNotFound) {
		return amount, nil
	}
	if err != nil {
		return 0, err
	}
	if !prof.IssueInvoices || prof.PricesIncludeTax {
		return amount, nil
	}
	ent, err := store.GetTaxEntity(ctx, q, prof.TaxEntityID)
	if err != nil {
		return 0, err
	}
	sup := tax.Supplier{GSTIN: ent.GSTIN, StateCode: ent.StateCode}
	if ent.LUTNumber != "" {
		sup.LUT = &tax.LUT{Number: ent.LUTNumber, ValidFrom: ent.LUTValidFrom, ValidTo: ent.LUTValidTo}
	}
	rcp := tax.Recipient{StateCode: buyer.StateCode, Country: buyer.Country}
	cls, err := tax.Classify(sup, rcp, s.nowUTC(), currency != "INR")
	if err != nil {
		// Bad buyer data must not stop a sale: the invoice treats the buyer as an
		// unregistered one in the supplier's State, and so do we.
		if cls, err = tax.Classify(sup, tax.Recipient{Country: buyer.Country}, s.nowUTC(), currency != "INR"); err != nil {
			return amount, nil
		}
	}
	amts, err := tax.FromTaxable(amount, prof.GSTRateBP, cls.InterState, cls.ZeroRated())
	if err != nil {
		return 0, newErr(ErrInvalid, "amount: %v", err)
	}
	return amts.Total, nil
}
