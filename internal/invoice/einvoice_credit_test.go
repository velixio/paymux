package invoice_test

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/invoice/einvoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestCreditNoteEInvoiceJSON(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setFX("USD", "2026-10-17", "83.2500")
		p, _ := e.pay(118000, "INR", b2bCustomer(t, "29ABCDE1234F1Z", "29", "5 Residency Road", "Bengaluru", "560025"), "pa_1")
		inv := only(t, e, p.ID)
		e.refund(p, 59000)
		cns := e.creditNotes(p.ID)
		if len(cns) != 1 {
			t.Fatalf("credit notes = %d", len(cns))
		}
		res, err := e.inv.CreditNoteEInvoice(ctx, cns[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := einvoice.Validate(res.JSON); err != nil {
			t.Fatalf("does not validate: %v\n%s", err, res.JSON)
		}
		d := doc(t, res.JSON)
		for path, want := range map[string]any{
			"DocDtls.Typ": "CRN", "DocDtls.No": cns[0].Number, "DocDtls.Dt": "17/10/2026", "TranDtls.SupTyp": "B2B",
			"ValDtls.AssVal": float64(500), "ValDtls.IgstVal": float64(90), "ValDtls.TotInvVal": float64(590),
			"BuyerDtls.Pos": "29",
		} {
			if got := field(d, strings.Split(path, ".")...); got != want {
				t.Errorf("%s = %v, want %v", path, got, want)
			}
		}
		ref := field(d, "RefDtls", "PrecDocDtls").([]any)[0].(map[string]any)
		if ref["InvNo"] != inv.Number || ref["InvDt"] != "17/10/2026" {
			t.Errorf("preceding document = %v", ref)
		}
		if !strings.Contains(res.Filename, "credit-note") || !strings.HasSuffix(res.Filename, ".json") {
			t.Errorf("filename %q", res.Filename)
		}

		// An export's credit note keeps the invoice's currency and rate.
		pe, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		e.refund(pe, 2500)
		cn2 := e.creditNotes(pe.ID)
		res, err = e.inv.CreditNoteEInvoice(ctx, cn2[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := einvoice.Validate(res.JSON); err != nil {
			t.Fatalf("export does not validate: %v\n%s", err, res.JSON)
		}
		de := doc(t, res.JSON)
		if field(de, "DocDtls", "Typ") != "CRN" || field(de, "TranDtls", "SupTyp") != "EXPWOP" ||
			field(de, "ValDtls", "TotInvValFc") != float64(25) || field(de, "ExpDtls", "ForCur") != "USD" {
			t.Errorf("export credit note = %s", res.JSON)
		}

		// A credit note of a consumer invoice has no e-invoice.
		pc, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.refund(pc, 10000)
		_, err = e.inv.CreditNoteEInvoice(ctx, e.creditNotes(pc.ID)[0].ID)
		if err == nil {
			t.Error("a B2C credit note got an e-invoice")
		}
		if _, err := e.inv.CreditNoteEInvoice(ctx, "crn_nosuch"); err == nil {
			t.Error("unknown credit note")
		}
	})
}
