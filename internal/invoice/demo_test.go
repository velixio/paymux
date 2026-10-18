package invoice_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/tax"
)

// The DEMO setup of the live server: a tax entity in Karnataka (GSTIN
// 29AAACD1234D1Z7), an app that issues invoices with prices including 18% GST.
// Whatever the buyer, a succeeded payment issues exactly one invoice whose
// total is what was received and whose heads add up.

const demoGSTIN = "29AAACD1234D1Z7"

func demoEnv(t *testing.T, db *store.DB, lut bool, inclusive bool) *env {
	t.Helper()
	e := newEnv(t, db, func(en *store.TaxEntity, p *store.AppTaxProfile) {
		en.LegalName, en.TradeName, en.GSTIN, en.PAN, en.StateCode = "DEMO Private Limited", "DEMO", demoGSTIN, "AAACD1234D", "29"
		en.Address = "12 MG Road, Bengaluru 560001"
		if !lut {
			en.LUTNumber = ""
			en.LUTValidFrom, en.LUTValidTo = time.Time{}, time.Time{}
		}
		p.PricesIncludeTax = inclusive
	})
	return e
}

// paid creates the payment through the core and pays what is asked.
func paidInvoice(t *testing.T, e *env, amount int64, currency string, cust *core.CustomerInput) (store.Payment, store.Invoice) {
	t.Helper()
	p, _ := e.pay(amount, currency, cust, "pa_1")
	e.runJobs()
	invs := e.invoices(p.ID)
	if len(invs) != 1 {
		t.Fatalf("a succeeded payment issued %d invoices, want exactly one", len(invs))
	}
	inv := invs[0]
	if inv.Total != p.AmountReceived {
		t.Errorf("invoice total %d != amount received %d", inv.Total, p.AmountReceived)
	}
	if inv.Taxable+inv.CGST+inv.SGST+inv.IGST != inv.Total {
		t.Errorf("heads %d+%d+%d+%d do not add up to %d", inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total)
	}
	if inv.PDFBlobID == "" {
		t.Error("no PDF was rendered")
	}
	return p, inv
}

func TestDemoEntityIsValid(t *testing.T) {
	if err := tax.ValidGSTIN(demoGSTIN); err != nil {
		t.Fatalf("the demo GSTIN: %v", err)
	}
	if tax.StateFromGSTIN(demoGSTIN) != "29" {
		t.Error("the demo GSTIN is not Karnataka")
	}
}

func TestDemoDomesticInvoices(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := demoEnv(t, db, true, true)
		karnataka := &core.CustomerInput{Name: "Asha", Billing: store.Billing{StateCode: "29", Country: "IN"}}
		_, inv := paidInvoice(t, e, 118000, "INR", karnataka)
		if inv.SupplyType != "b2c" || inv.PlaceOfSupply != "29" || inv.InterState || inv.Taxable != 100000 || inv.CGST != 9000 || inv.SGST != 9000 || inv.IGST != 0 {
			t.Errorf("Karnataka buyer: %+v", inv)
		}
		maha := &core.CustomerInput{Name: "Ravi", Billing: store.Billing{StateCode: "27", Country: "IN"}}
		_, inv = paidInvoice(t, e, 118000, "INR", maha)
		if inv.SupplyType != "b2c" || inv.PlaceOfSupply != "27" || !inv.InterState || inv.Taxable != 100000 || inv.IGST != 18000 || inv.CGST != 0 || inv.SGST != 0 {
			t.Errorf("out-of-state buyer: %+v", inv)
		}
		// A buyer with a GSTIN: B2B, by the GSTIN's state.
		sameState := &core.CustomerInput{Name: "Acme", Billing: store.Billing{GSTIN: validGSTIN(t, "29ABCDE1234F1Z"), LegalName: "Acme Pvt Ltd", StateCode: "29", Country: "IN"}}
		_, inv = paidInvoice(t, e, 118000, "INR", sameState)
		if inv.SupplyType != "b2b" || inv.InterState || inv.CGST != 9000 || inv.SGST != 9000 {
			t.Errorf("B2B in Karnataka: %+v", inv)
		}
		otherState := &core.CustomerInput{Name: "Acme", Billing: store.Billing{GSTIN: validGSTIN(t, "27ABCDE1234F1Z"), LegalName: "Acme Pvt Ltd", StateCode: "27", Country: "IN"}}
		_, inv = paidInvoice(t, e, 118000, "INR", otherState)
		if inv.SupplyType != "b2b" || !inv.InterState || inv.IGST != 18000 {
			t.Errorf("B2B out of state: %+v", inv)
		}
		// A buyer who gave no State is taken to be in the supplier's State and flagged.
		_, inv = paidInvoice(t, e, 118000, "INR", &core.CustomerInput{Name: "Nina"})
		if inv.InterState || inv.CGST != 9000 || !inv.HasFlag("pos_defaulted") {
			t.Errorf("no State: %+v", inv)
		}
		// An odd amount still adds up.
		_, inv = paidInvoice(t, e, 99999, "INR", karnataka)
		if inv.Taxable+inv.CGST+inv.SGST != 99999 {
			t.Errorf("odd amount: %+v", inv)
		}
	})
}

func TestDemoExports(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		us := &core.CustomerInput{Name: "Bob", Billing: store.Billing{Country: "US", Line1: "1 Main St", City: "Austin"}}
		// With a LUT: zero-rated, no tax.
		e := demoEnv(t, db, true, true)
		e.setFX("USD", "2026-10-17", "83.2500")
		_, inv := paidInvoice(t, e, 5000, "USD", us)
		if inv.SupplyType != "export_lut" || inv.IGST != 0 || inv.Taxable != 5000 || inv.LUTNumber == "" || inv.PlaceOfSupply != "OUTSIDE_INDIA" {
			t.Errorf("export under LUT: %+v", inv)
		}
		// A buyer abroad paying in rupees is not under the LUT: IGST, flagged.
		_, inv = paidInvoice(t, e, 118000, "INR", us)
		if inv.SupplyType != "export_igst" || inv.IGST != 18000 || !inv.HasFlag("export_igst") {
			t.Errorf("rupees from abroad: %+v", inv)
		}
	})
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		// No LUT on the entity: the export pays IGST.
		e := demoEnv(t, db, false, true)
		e.setFX("USD", "2026-10-17", "83.2500")
		_, inv := paidInvoice(t, e, 11800, "USD", &core.CustomerInput{Name: "Bob", Billing: store.Billing{Country: "US", Line1: "1 Main St", City: "Austin"}})
		if inv.SupplyType != "export_igst" || inv.IGST != 1800 || inv.Taxable != 10000 || !inv.HasFlag("export_igst") {
			t.Errorf("export without a LUT: %+v", inv)
		}
	})
}

// With prices excluding tax the core charges taxable + GST; the invoice is still
// the amount received.
func TestDemoExclusivePricing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := demoEnv(t, db, true, false)
		p, inv := paidInvoice(t, e, 100000, "INR", &core.CustomerInput{Name: "Asha", Billing: store.Billing{StateCode: "29", Country: "IN"}})
		if p.AmountReceived != 118000 || inv.Taxable != 100000 || inv.CGST != 9000 || inv.SGST != 9000 {
			t.Errorf("payment %d, invoice %+v", p.AmountReceived, inv)
		}
	})
}

// Success is told more than once, or by two attempts: still one invoice.
func TestDemoOnePaymentIsOneInvoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := demoEnv(t, db, true, true)
		cust := &core.CustomerInput{Name: "Asha", Billing: store.Billing{StateCode: "29", Country: "IN"}}
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 118000, Currency: "INR", Customer: cust})
		if err != nil {
			t.Fatal(err)
		}
		a1, _ := e.core.CreateAttempt(ctx, res.Payment.ID, "pa_1")
		a2, _ := e.core.CreateAttempt(ctx, res.Payment.ID, "pa_1")
		snap := core.Snapshot{Status: state.AttemptCaptured, Amount: 118000, Currency: "INR", Method: "upi"}
		for _, id := range []string{a1.ID, a1.ID, a2.ID} { // a repeat, then a duplicate payment
			snap.ProviderPaymentID = "pp_" + id
			if _, err := e.core.ApplyAttemptSnapshot(ctx, id, snap); err != nil {
				t.Fatal(err)
			}
		}
		e.runJobs()
		invs := e.invoices(res.Payment.ID)
		if len(invs) != 1 || invs[0].AttemptID != a1.ID {
			t.Fatalf("invoices = %+v", invs)
		}
		if n := e.mailer.count(); n > 1 {
			t.Errorf("%d invoice emails", n)
		}
	})
}
