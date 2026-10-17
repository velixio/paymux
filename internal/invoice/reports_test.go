package invoice_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/tax"
)

func day(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }

// octoberWorld issues the documents of the golden report on both databases.
func octoberWorld(t *testing.T, db *store.DB) *env {
	e := newEnv(t, db, func(en *store.TaxEntity, _ *store.AppTaxProfile) {
		en.LUTValidTo = time.Date(2026, 10, 9, 0, 0, 0, 0, tax.IST) // inclusive: valid on the 9th, expired on the 10th
	})
	buyer := validGSTIN(t, "29ABCDE1234F1Z")
	b2b := &core.CustomerInput{Name: "Acme Pvt Ltd", Billing: store.Billing{LegalName: "Acme Pvt Ltd", GSTIN: buyer, Country: "IN"}}

	e.setNow(day(3))
	p1, _ := e.pay(118000, "INR", intraB2C, "pa_1") // B2CS, Maharashtra
	e.setNow(day(5))
	p2, _ := e.pay(236000, "INR", b2b, "pa_1") // B2B, inter-state
	e.setNow(day(7))
	p3, _ := e.pay(15000000, "INR", interB2C, "pa_1") // B2CL: Rs 1,50,000 inter-state B2C
	e.setNow(day(9))
	e.setFX("USD", "2026-10-09", "83.2500")
	p4, _ := e.pay(5000, "USD", usBuyer, "pa_1") // export under LUT
	e.setNow(day(10))
	e.setFX("USD", "2026-10-10", "83.5000")
	e.pay(5000, "USD", usBuyer, "pa_1") // LUT expired: export with IGST
	e.setNow(day(12))
	e.pay(5000, "USD", usBuyer, "pa_1") // no FX rate that day: fx_pending
	e.setNow(day(14))
	e.pay(118000, "INR", noState, "pa_1") // pos_defaulted
	e.setNow(day(15))
	e.refund(p1, 30000)
	e.setNow(day(16))
	e.refund(p2, 118000)
	e.setNow(day(17))
	e.refund(p3, 5000000)
	e.setNow(day(18))
	e.refund(p4, 1000)
	return e
}

func TestMonthlyReportGolden(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := octoberWorld(t, db)
		set, err := e.inv.Reports(ctx, "te_1", "2026-10")
		if err != nil {
			t.Fatal(err)
		}
		if set.Month != "2026-10" || set.Entity.GSTIN != supplierGSTIN {
			t.Fatalf("set = %+v", set.Entity)
		}
		var names []string
		for _, f := range set.Files {
			names = append(names, f.Name)
			golden := filepath.Join("testdata", "reports", "2026-10", f.Name)
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, f.CSV, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with UPDATE_GOLDEN=1 to create it)", err)
			}
			if string(want) != string(f.CSV) {
				t.Errorf("%s differs from the golden file\n--- got ---\n%s\n--- want ---\n%s", f.Name, f.CSV, want)
			}
		}
		wantNames := []string{"sales_register.csv", "b2b.csv", "b2cl.csv", "b2cs.csv", "exp.csv", "cdnr.csv", "cdnur.csv",
			"sac_summary.csv", "documents_issued.csv", "exceptions.csv", "settlements.csv", "itc_fees.csv"}
		if strings.Join(names, ",") != strings.Join(wantNames, ",") {
			t.Errorf("files = %v", names)
		}
	})
}

func TestMonthlyReportEdges(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := octoberWorld(t, db)
		// A month with no documents still yields every sheet, header only.
		set, err := e.inv.Reports(ctx, "te_1", "2026-11")
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range set.Files {
			if n := strings.Count(string(f.CSV), "\n"); n != 1 {
				t.Errorf("%s has %d lines, want just the header", f.Name, n)
			}
		}
		if _, err := e.inv.Reports(ctx, "te_1", "2026-13"); err == nil {
			t.Error("bad month accepted")
		}
		if _, err := e.inv.Reports(ctx, "te_nosuch", "2026-10"); err == nil {
			t.Error("unknown entity accepted")
		}
		// Month boundaries are IST: a document issued at 18:31 UTC on 31 Oct
		// is already 1 November.
		e.setNow(time.Date(2026, 10, 31, 18, 31, 0, 0, time.UTC))
		e.pay(11800, "INR", intraB2C, "pa_1")
		oct, _ := e.inv.Reports(ctx, "te_1", "2026-10")
		nov, _ := e.inv.Reports(ctx, "te_1", "2026-11")
		if strings.Count(string(nov.Files[0].CSV), "\n") != 2 {
			t.Errorf("november register:\n%s", nov.Files[0].CSV)
		}
		if strings.Contains(string(oct.Files[0].CSV), "000008") {
			t.Error("the 1 November (IST) invoice leaked into October")
		}
		// Written to disk.
		dir := t.TempDir()
		if err := oct.Write(filepath.Join(dir, "out")); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "out", "b2b.csv")); err != nil || len(b) == 0 {
			t.Errorf("written file: %v", err)
		}
	})
}
