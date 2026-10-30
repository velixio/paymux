package invoice_test

import (
	"testing"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// exclusivePay creates a payment for `amount` in an app whose prices exclude
// tax and pays whatever the core says is due.
func exclusivePay(t *testing.T, e *env, amount int64, currency string, cust *core.CustomerInput) (store.Payment, store.Invoice) {
	t.Helper()
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: amount, Currency: currency, Customer: cust})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.core.CreateAttempt(ctx, res.Payment.ID, "pa_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: res.Payment.Amount,
		Currency: currency, Method: "upi", ProviderPaymentID: "pp_" + a.ID}); err != nil {
		t.Fatal(err)
	}
	p, err := e.core.GetPayment(ctx, e.scope, res.Payment.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, only(t, e, p.ID)
}

// With prices_include_tax=false the amount of a payment is the taxable value:
// the core charges taxable + GST, so the invoice total is what was received.
func TestTaxExclusiveChargesTaxablePlusGST(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(_ *store.TaxEntity, p *store.AppTaxProfile) { p.PricesIncludeTax = false })
		cases := []struct {
			name          string
			amount        int64
			cust          *core.CustomerInput
			wantCharged   int64
			taxable, igst int64
			cgst, sgst    int64
		}{
			{"intra-state", 100000, intraB2C, 118000, 100000, 0, 9000, 9000},
			{"inter-state", 100000, interB2C, 118000, 100000, 18000, 0, 0},
			{"rounding", 99999, intraB2C, 117999, 99999, 0, 9000, 9000},
		}
		for _, c := range cases {
			p, inv := exclusivePay(t, e, c.amount, "INR", c.cust)
			if p.Amount != c.wantCharged || p.AmountReceived != c.wantCharged {
				t.Errorf("%s: charged %d / received %d, want %d", c.name, p.Amount, p.AmountReceived, c.wantCharged)
			}
			if inv.Total != p.AmountReceived {
				t.Errorf("%s: invoice total %d != amount received %d", c.name, inv.Total, p.AmountReceived)
			}
			if inv.Taxable != c.taxable || inv.IGST != c.igst || inv.CGST+inv.SGST != c.cgst+c.sgst {
				t.Errorf("%s: invoice = taxable %d cgst %d sgst %d igst %d", c.name, inv.Taxable, inv.CGST, inv.SGST, inv.IGST)
			}
		}
	})
}

func TestTaxExclusiveAddsNoTaxToAZeroRatedExport(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(_ *store.TaxEntity, p *store.AppTaxProfile) { p.PricesIncludeTax = false })
		e.setFX("USD", "2026-10-17", "83.10")
		p, inv := exclusivePay(t, e, 5000, "USD", usBuyer) // LUT valid: zero-rated
		if p.Amount != 5000 || inv.Total != 5000 || inv.IGST != 0 {
			t.Errorf("export: charged %d, invoice %+v", p.Amount, inv)
		}
	})
}

// Tax is not added when prices already include it.
func TestAmountIsUntouchedWhenPricesIncludeTax(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 118000, Currency: "INR", Customer: intraB2C})
		if err != nil || res.Payment.Amount != 118000 {
			t.Fatalf("inclusive: %+v %v", res.Payment, err)
		}
	})
}

// Tax is only added when there is an invoice to carry it.
func TestAmountIsUntouchedWhenThereIsNoInvoicing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(_ *store.TaxEntity, p *store.AppTaxProfile) { p.PricesIncludeTax = false; p.IssueInvoices = false })
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100000, Currency: "INR", Customer: intraB2C})
		if err != nil || res.Payment.Amount != 100000 {
			t.Fatalf("no invoicing: %+v %v", res.Payment, err)
		}
	})
}
