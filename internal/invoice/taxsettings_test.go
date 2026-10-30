package invoice_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func (e *env) setOrg(key, raw string) {
	e.t.Helper()
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, json.RawMessage(raw)); err != nil {
		e.t.Fatal(err)
	}
}

// The credit-note cutoff is an org setting; the statutory 30 November after
// the end of the financial year is its default.
func TestCreditNoteCutoffIsASetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1") // supplied 17 Oct 2026 (FY 2026-27)

		e.setOrg("credit_note_cutoff", `"12-31"`)               // the cutoff moves to 31 December 2027
		e.setNow(time.Date(2027, 12, 15, 6, 0, 0, 0, time.UTC)) // after 30 Nov, before 31 Dec
		in := e.refund(p, 10000)
		e.setNow(time.Date(2028, 1, 1, 6, 0, 0, 0, time.UTC)) // after the new cutoff
		late := e.refund(p, 20000)
		cns := e.creditNotes(p.ID)
		if len(cns) != 1 || cns[0].RefundID != in.ID {
			t.Fatalf("credit notes = %+v", cns)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			if f, _ := store.RefundFlags(ctx, q, late.ID); len(f) != 1 || f[0] != "credit_note_cutoff_passed" {
				t.Errorf("late refund flags = %v", f)
			}
			return nil
		})

		// "off": no cutoff at all.
		e.setOrg("credit_note_cutoff", `"off"`)
		e.setNow(time.Date(2031, 6, 1, 6, 0, 0, 0, time.UTC))
		e.refund(p, 30000)
		if n := len(e.creditNotes(p.ID)); n != 2 {
			t.Errorf("credit notes with the cutoff off = %d, want 2", n)
		}
	})
}

// The B2CL threshold is an org setting (Rs 1,00,000 by default).
func TestB2CLThresholdIsASetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := octoberWorld(t, db)
		b2cl := func() int {
			set, err := e.inv.Reports(ctx, "te_1", "2026-10")
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range set.Files {
				if f.Name == "b2cl.csv" {
					return strings.Count(strings.TrimSpace(string(f.CSV)), "\n") // data rows
				}
			}
			t.Fatal("no b2cl.csv")
			return 0
		}
		if n := b2cl(); n != 1 {
			t.Fatalf("default threshold: %d b2cl rows, want 1 (the Rs 1,50,000 invoice)", n)
		}
		e.setOrg("b2cl_threshold", `20000000`) // Rs 2,00,000
		if n := b2cl(); n != 0 {
			t.Errorf("raised threshold: %d b2cl rows, want 0", n)
		}
	})
}
