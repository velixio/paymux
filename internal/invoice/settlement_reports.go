package invoice

import (
	"fmt"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
)

// settlementsSheet lists the provider settlements that paid out payments of
// the tax entity's apps: what the provider paid, the fees it kept and the GST
// on them, and whether the settlement's fee totals matched the sum of the
// payments' fees (spec section 11).
func settlementsSheet(rows []store.SettlementFees) csvTable {
	t := csvTable{name: "settlements.csv"}
	t.add("settlement_id", "provider", "account", "utr", "status", "settled_date", "currency", "amount", "fees", "fee_gst",
		"payments", "payment_fees", "payment_fee_gst", "fee_mismatch")
	for _, r := range rows {
		s, cur := r.Settlement, r.Settlement.Currency
		t.add(s.ProviderSettlementID, r.Connector, r.AccountName, s.UTR, s.Status, istDate(s.SettledAt), cur,
			minorStr(s.Amount, cur), minorStr(s.Fees, cur), minorStr(s.FeeTax, cur), fmt.Sprint(r.Attempts),
			minorStr(r.Fee, cur), minorStr(r.FeeTax, cur), yesNo(s.FeeMismatch))
	}
	return t
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// itcSheet is the provider fee input-tax-credit register: for each settlement,
// the fees the provider charged the entity and the GST on them, split into
// IGST, or CGST and SGST when the provider is registered in the entity's own
// state, with whether the entity can claim the credit. The provider's GSTIN
// comes from the provider account.
func itcSheet(ent store.TaxEntity, rows []store.SettlementFees) csvTable {
	t := csvTable{name: "itc_fees.csv"}
	t.add("provider_gstin", "provider", "account", "document_ref", "date", "currency", "taxable_fee", "igst", "cgst", "sgst",
		"total_gst", "itc_status")
	for _, r := range rows {
		s, cur := r.Settlement, r.Settlement.Currency
		status, igst, cgst, sgst := itcStatus(ent, r)
		t.add(r.ProviderGSTIN, r.Connector, r.AccountName, s.ProviderSettlementID+" ("+s.UTR+")", istDate(s.SettledAt), cur,
			minorStr(r.Fee, cur), minorStr(igst, cur), minorStr(cgst, cur), minorStr(sgst, cur), minorStr(r.FeeTax, cur), status)
	}
	return t
}

func itcStatus(ent store.TaxEntity, r store.SettlementFees) (status string, igst, cgst, sgst int64) {
	switch {
	case r.Settlement.Currency != "INR":
		return "not_inr", 0, 0, 0
	case r.FeeTax == 0:
		return "no_gst", 0, 0, 0
	case r.ProviderGSTIN == "":
		return "missing_provider_gstin", 0, 0, 0
	case tax.ValidGSTIN(r.ProviderGSTIN) != nil:
		return "invalid_provider_gstin", 0, 0, 0
	}
	if tax.StateFromGSTIN(r.ProviderGSTIN) == ent.StateCode {
		cgst = r.FeeTax / 2
		return "claimable", 0, cgst, r.FeeTax - cgst
	}
	return "claimable", r.FeeTax, 0, 0
}
