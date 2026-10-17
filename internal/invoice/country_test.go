package invoice_test

import (
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// A buyer abroad paying in rupees is not a domestic B2C sale: the buyer's
// country (kept on the customer) makes it an export taxed with IGST, flagged
// for the operator because no LUT covers a payment in INR.
func TestForeignBuyerPayingInINRIsAnExport(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		abroad := &core.CustomerInput{Name: "Bob", Billing: store.Billing{Country: "US"}}
		p, _ := e.pay(118000, "INR", abroad, "pa_1")
		inv := only(t, e, p.ID)
		if inv.SupplyType != "export_igst" || inv.PlaceOfSupply != "OUTSIDE_INDIA" || !inv.HasFlag("export_igst") || inv.CGST != 0 || inv.SGST != 0 {
			t.Errorf("invoice = %+v", inv)
		}
	})
}
