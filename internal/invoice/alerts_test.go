package invoice_test

import (
	"slices"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/tax"
)

// Tax exceptions are listed in the monthly report, but nobody reads a report
// in time: each one also raises an ops alert, once per document.

func (e *env) alertKinds() []string {
	e.runJobs()
	k := e.alerts.kinds()
	slices.Sort(k)
	return k
}

func TestTaxExceptionsRaiseOpsAlerts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(en *store.TaxEntity, _ *store.AppTaxProfile) {
			en.LUTValidTo = time.Date(2026, 10, 9, 0, 0, 0, 0, tax.IST) // expired on the 17th
		})
		// An ordinary sale raises nothing.
		e.pay(118000, "INR", intraB2C, "pa_1")
		if k := e.alertKinds(); len(k) != 0 {
			t.Fatalf("an ordinary sale raised %v", k)
		}
		e.pay(118000, "INR", noState, "pa_1")
		if k := e.alertKinds(); !slices.Equal(k, []string{"tax.pos_defaulted"}) {
			t.Errorf("no State: %v", k)
		}
		e.pay(5000, "USD", usBuyer, "pa_1") // LUT expired: export with IGST, and no FX rate for the day
		k := e.alertKinds()
		for _, want := range []string{"tax.export_igst", "tax.fx_pending"} {
			if !slices.Contains(k, want) {
				t.Errorf("missing %s in %v", want, k)
			}
		}
	})
}

func TestCreditNoteCutoffRaisesAnOpsAlert(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.setNow(time.Date(2027, 12, 2, 6, 0, 0, 0, time.UTC)) // after 30 Nov 2027
		e.refund(p, 10000)
		if k := e.alertKinds(); !slices.Equal(k, []string{"tax.credit_note_cutoff_passed"}) {
			t.Errorf("alerts = %v", k)
		}
		e.refund(p, 20000)
		e.runJobs()
		if n := len(e.alerts.kinds()); n != 2 {
			t.Errorf("each refund that misses the cutoff raises its own alert: %d alerts", n)
		}
	})
}

// The alerts carry what an operator needs to find the document.
func TestTaxAlertsNameTheDocument(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", noState, "pa_1")
		e.runJobs()
		inv := only(t, e, p.ID)
		if len(e.alerts.got) != 1 {
			t.Fatalf("alerts = %+v", e.alerts.got)
		}
		al := e.alerts.got[0]
		if al.Refs["invoice"] != inv.ID || al.Refs["payment"] != p.ID || al.Message == "" {
			t.Errorf("alert = %+v", al)
		}
	})
}
