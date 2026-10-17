package subscriptions_test

import (
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// A notice announces a debit on one mandate. If the mandate is replaced
// before the debit, the new mandate gets its own notice and the debit waits
// for it (the shared invariant of env_test.go would fail the test otherwise).
func TestMandateSwapAfterTheNoticeNeedsANewNotice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		old := e.mandate(sub.MandateID)
		e.advanceTo(utc(2027, 2, 26, 4, 0)) // notice for the old mandate
		if len(e.r.notices) != 1 || e.r.notices[0].Mandate.ProviderMandateID != "pm_"+old.ID {
			t.Fatalf("notices = %+v", e.r.notices)
		}
		res, err := e.subs.UpdatePaymentMethod(ctx, e.scope, sub.ID, "card")
		if err != nil {
			t.Fatal(err)
		}
		_ = res
		pending := e.mandate(e.sub(sub.ID).PendingMandateID)
		e.advanceTo(utc(2027, 2, 27, 4, 0))
		e.activateMandateByID(pending, "pm_card_new")
		e.runJobs()
		if len(e.r.notices) != 2 || e.r.notices[1].Mandate.ProviderMandateID != "pm_card_new" {
			t.Fatalf("notices = %+v", e.r.notices)
		}
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		if e.r.chargeCount() != 1 || e.r.charges[0].Mandate.ProviderMandateID != "pm_card_new" {
			t.Fatalf("charges = %+v", e.r.charges)
		}
		if got := e.sub(sub.ID); got.RenewalsDone != 2 || got.Status != "active" {
			t.Errorf("sub = %+v", got)
		}
	})
}
