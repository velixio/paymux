package subscriptions_test

import (
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
)

// A provider refuses charges below its minimum. A proration credit therefore
// never takes a renewal under it: the part of the credit that does not fit
// waits for the renewals after.
func TestProrationCreditNeverTakesARenewalBelowTheProvidersMinimum(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.minCharge = map[string]int64{"INR": 5000}
		sub := active(t, e, nil)
		e.advanceTo(t0.AddDate(0, 0, 1))
		update(t, e, sub.ID, 10000, subscriptions.ProrationNextInvoice)
		credit := e.sub(sub.ID).ProrationAdjust
		if credit > -30000 {
			t.Fatalf("credit = %d", credit)
		}
		renew(e, periodEnd)
		if got := e.r.charges[0].Amount; got != 5000 {
			t.Fatalf("the first renewal charged %d, want the provider's minimum 5000", got)
		}
		if left := e.sub(sub.ID).ProrationAdjust; left != credit+5000 {
			t.Errorf("carried credit = %d, want %d", left, credit+5000)
		}
	})
}

// An immediate upgrade charge below the minimum cannot be made on its own: it
// is added to the next renewal.
func TestProrationImmediateChargeBelowTheMinimumWaitsForTheNextRenewal(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.minCharge = map[string]int64{"INR": 5000}
		sub := active(t, e, nil)
		e.advanceTo(day7)
		res := update(t, e, sub.ID, 50000, subscriptions.ProrationImmediate) // 49,900 -> 50,000: about 75 for the rest of the period
		o := res.Proration
		if o == nil || o.Mode != subscriptions.ProrationNextInvoice || o.Reason != "below_minimum_charge" || o.ChargeAt != nil || o.Amount <= 0 {
			t.Fatalf("outcome = %+v", o)
		}
		if got := e.sub(sub.ID); got.ProrationAdjust != o.Amount {
			t.Errorf("the small difference must wait for the next renewal: %+v", got)
		}
		e.runJobs()
		if e.r.chargeCount() != 0 || len(e.r.notices) != 0 {
			t.Errorf("a charge below the minimum was announced or made: %d charges, %d notices", e.r.chargeCount(), len(e.r.notices))
		}
	})
}
