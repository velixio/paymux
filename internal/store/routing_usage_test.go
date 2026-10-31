package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestRoutingUsageIncludesLegacyChargeWithoutAttemptID(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		if err := exec(t, db, insPayment, "pay_usage", 100, 0); err != nil {
			t.Fatal(err)
		}
		if err := exec(t, db, `INSERT INTO payment_attempts
			(id, org_id, mode, payment_id, provider_account_id, provider_ref, status, amount_received, created_at, updated_at)
			VALUES ('att_usage', 'o', 'test', 'pay_usage', 'pa_usage', 'provider_order', 'captured', 100, 10, 10)`); err != nil {
			t.Fatal(err)
		}
		if err := exec(t, db, `INSERT INTO ledger_entries
			(id, org_id, mode, kind, amount, currency, payment_id, occurred_at, attempt_id)
			VALUES ('led_usage', 'o', 'test', 'charge', 100, 'INR', 'pay_usage', 10, NULL)`); err != nil {
			t.Fatal(err)
		}
		var sales, amount int64
		err := db.Read(context.Background(), func(q store.Querier) (err error) {
			sales, amount, err = store.RoutingUsage(context.Background(), q, []string{"pa_usage"}, "INR", time.UnixMilli(1))
			return err
		})
		if err != nil || sales != 1 || amount != 100 {
			t.Fatalf("usage sales=%d amount=%d: %v", sales, amount, err)
		}
	})
}
