package subscriptions_test

import (
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// The provider calls of a renewal (the pre-debit notice and the charge) run
// under the ids of what they are about, so a failed call in the error log can be
// traced to the subscription, mandate, payment and attempt.
func TestRenewalCallsCarryTheirIDsForTheErrorLog(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		e.runJobs()
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		e.r.mu.Lock()
		defer e.r.mu.Unlock()
		seen := map[string]bool{}
		for _, ci := range e.r.callIDs {
			seen[ci.Call] = true
			if ci.IDs["subscription_id"] != sub.ID || ci.IDs["mandate_id"] == "" || ci.IDs["provider_account_id"] == "" || !strings.HasPrefix(ci.IDs["job_id"], "job_") {
				t.Errorf("%s ran under %v", ci.Call, ci.IDs)
			}
			if ci.Call == "charge" && (!strings.HasPrefix(ci.IDs["attempt_id"], "att_") || !strings.HasPrefix(ci.IDs["payment_id"], "pay_")) {
				t.Errorf("the charge lacks its attempt and payment: %v", ci.IDs)
			}
		}
		if !seen["notify"] || !seen["charge"] {
			t.Errorf("calls seen: %v", seen)
		}
	})
}
