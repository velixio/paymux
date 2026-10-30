package invoice_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestPartialCreditNotesSumToTheInvoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		inv := only(t, e, p.ID)
		var refunds []store.Refund
		for _, amt := range []int64{30001, 50003, 37996} { // sums to 118000, none divides evenly
			refunds = append(refunds, e.refund(p, amt))
		}
		cns := e.creditNotes(p.ID)
		if len(cns) != 3 {
			t.Fatalf("credit notes = %d", len(cns))
		}
		var taxable, cgst, sgst, igst, total int64
		for i, c := range cns {
			if c.Number != []string{"CN/2627/000001", "CN/2627/000002", "CN/2627/000003"}[i] || c.RefundID != refunds[i].ID ||
				c.InvoiceID != inv.ID || c.Total != refunds[i].Amount || c.Taxable+c.CGST+c.SGST+c.IGST != c.Total || c.Reason != "customer request" {
				t.Errorf("credit note %d = %+v", i, c)
			}
			taxable, cgst, sgst, igst, total = taxable+c.Taxable, cgst+c.CGST, sgst+c.SGST, igst+c.IGST, total+c.Total
		}
		if taxable != inv.Taxable || cgst != inv.CGST || sgst != inv.SGST || igst != inv.IGST || total != inv.Total {
			t.Errorf("credit notes sum to %d/%d/%d/%d/%d, invoice is %d/%d/%d/%d/%d",
				taxable, cgst, sgst, igst, total, inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total)
		}
		if got := e.eventsOf("credit_note.issued"); len(got) != 3 || got[0].ObjectType != "credit_note" {
			t.Errorf("events = %v", e.emittedTypes())
		}
	})
}

func TestInterStateCreditNoteCreditsIGST(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", interB2C, "pa_1")
		e.refund(p, 59000)
		cns := e.creditNotes(p.ID)
		if len(cns) != 1 || cns[0].IGST != 9000 || cns[0].CGST != 0 || cns[0].Taxable != 50000 || cns[0].Total != 59000 {
			t.Fatalf("credit notes = %+v", cns)
		}
	})
}

func TestCreditNoteIsIdempotentPerRefund(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		r := e.refund(p, 10000)
		if err := db.Write(ctx, func(tx store.Tx) error { return e.core.RefundSucceeded(ctx, tx, r) }); err != nil {
			t.Fatal(err)
		}
		if n := len(e.creditNotes(p.ID)); n != 1 {
			t.Errorf("credit notes = %d, want 1", n)
		}
	})
}

func TestNoCreditNoteAfterTheCutoff(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		// Supply on 17 Oct 2026 (FY 2026-27): the cutoff is 30 Nov 2027 23:59:59 IST.
		e.setNow(time.Date(2027, 11, 30, 18, 29, 0, 0, time.UTC)) // 23:59 IST, still inside
		in := e.refund(p, 10000)
		e.setNow(time.Date(2027, 11, 30, 18, 31, 0, 0, time.UTC)) // 00:01 IST on 1 Dec
		late := e.refund(p, 20000)
		cns := e.creditNotes(p.ID)
		if len(cns) != 1 || cns[0].RefundID != in.ID {
			t.Fatalf("credit notes = %+v", cns)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			if f, err := store.RefundFlags(ctx, q, late.ID); err != nil || len(f) != 1 || f[0] != "credit_note_cutoff_passed" {
				t.Errorf("late refund flags = %v err=%v", f, err)
			}
			if f, _ := store.RefundFlags(ctx, q, in.ID); len(f) != 0 {
				t.Errorf("in-time refund flags = %v", f)
			}
			return nil
		})
	})
}

func TestNoCreditNoteWithoutAnInvoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_mor")
		e.refund(p, 10000)
		if n := len(e.creditNotes(p.ID)); n != 0 {
			t.Errorf("credit notes = %d", n)
		}
	})
}
