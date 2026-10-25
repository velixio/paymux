package httpapi_test

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Security remediation R-A5 / H-1. The middleware reservation is only a fast
// path: a request that outlives idempotencyStale loses it to a retry. These
// tests drop the reservation of a request whose effect has committed, which is
// what that takeover amounts to, and repeat the request under the same key.
func (f *fixture) forgetReservations() {
	f.t.Helper()
	if err := f.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM idempotency_keys")
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestIdempotencyKeyOnThePayoutSurvivesALostReservation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newPayoutFixture(t, db, `["payout"]`)
		w := f.scopedKey("test", authz.ScopePayoutsWrite)
		first := f.do("POST", "/v1/payouts", w, payoutBody, "Idempotency-Key", "po-lost-1")
		if first.status != 201 {
			t.Fatalf("create: %d %s", first.status, first.raw)
		}
		f.forgetReservations()
		again := f.do("POST", "/v1/payouts", w, payoutBody, "Idempotency-Key", "po-lost-1")
		if again.status != 201 || again.json(t)["id"] != first.json(t)["id"] {
			t.Errorf("the retry answered %d %s, want the first payout", again.status, again.raw)
		}
		if n := f.count("SELECT COUNT(*) FROM payouts"); n != 1 {
			t.Fatalf("one Idempotency-Key made %d payouts", n)
		}
		// The key names one request: another body under it is refused.
		other := map[string]any{"amount": 1, "currency": "INR",
			"beneficiary": map[string]any{"name": "Asha Rao", "account_number": "123456789012", "ifsc": "HDFC0001234"}}
		f.forgetReservations()
		if r := f.do("POST", "/v1/payouts", w, other, "Idempotency-Key", "po-lost-1"); r.status != 409 {
			t.Errorf("another amount under the same key: %d %s", r.status, r.raw)
		}
		if n := f.count("SELECT COUNT(*) FROM payouts"); n != 1 {
			t.Fatalf("a reused key made %d payouts", n)
		}
		// Nor the same amount to another destination: the client would believe
		// its new payout was made when the old one is what it gets back.
		elsewhere := func(b map[string]any) map[string]any {
			out := map[string]any{"amount": payoutBody["amount"], "currency": "INR", "narration": "other words", "beneficiary": b}
			return out
		}
		for name, b := range map[string]map[string]any{
			"account": {"name": "Asha Rao", "account_number": "999999999999", "ifsc": "HDFC0001234"},
			"ifsc":    {"name": "Asha Rao", "account_number": "123456789012", "ifsc": "ICIC0001234"},
			"upi":     {"name": "Asha Rao", "upi": "asha@okbank"},
		} {
			f.forgetReservations()
			if r := f.do("POST", "/v1/payouts", w, elsewhere(b), "Idempotency-Key", "po-lost-1"); r.status != 409 || r.errType(t) != "idempotency_key_reuse" {
				t.Errorf("another %s under the same key: %d %s", name, r.status, r.raw)
			}
		}
		// The same destination with other words is the same payout.
		f.forgetReservations()
		same := elsewhere(map[string]any{"name": "Asha Rao", "account_number": "123456789012", "ifsc": "HDFC0001234"})
		if r := f.do("POST", "/v1/payouts", w, same, "Idempotency-Key", "po-lost-1"); r.status != 201 || r.json(t)["id"] != first.json(t)["id"] {
			t.Errorf("same destination, other narration: %d %s", r.status, r.raw)
		}
		if n := f.count("SELECT COUNT(*) FROM payouts"); n != 1 {
			t.Fatalf("a reused key made %d payouts", n)
		}
	})
}

func TestIdempotencyKeyOnTheRefundSurvivesALostReservation(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		_, m := f.createPayment(createBody)
		id := m["id"].(string)
		f.succeed(id, 49900)
		path := "/v1/payments/" + id + "/refunds"
		first := f.do("POST", path, f.testKey, `{"amount":1000}`, "Idempotency-Key", "rf-lost-1")
		if first.status != 201 {
			t.Fatalf("refund: %d %s", first.status, first.raw)
		}
		f.forgetReservations()
		again := f.do("POST", path, f.testKey, `{"amount":1000}`, "Idempotency-Key", "rf-lost-1")
		if again.status != 201 || again.json(t)["id"] != first.json(t)["id"] {
			t.Errorf("the retry answered %d %s, want the first refund", again.status, again.raw)
		}
		if n := f.count("SELECT COUNT(*) FROM refunds"); n != 1 {
			t.Fatalf("one Idempotency-Key made %d refunds", n)
		}
		f.forgetReservations()
		if r := f.do("POST", path, f.testKey, `{"amount":2000}`, "Idempotency-Key", "rf-lost-1"); r.status != 409 {
			t.Errorf("another amount under the same key: %d %s", r.status, r.raw)
		}
		if n := f.count("SELECT COUNT(*) FROM refunds"); n != 1 {
			t.Fatalf("a reused key made %d refunds", n)
		}
	})
}

// The constraint itself: unique per app and mode, and keyless rows never collide.
func TestIdempotencyKeyColumnsAreUniquePerAppAndMode(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ins := map[string]string{
			"payouts": `INSERT INTO payouts (id, org_id, mode, app_id, provider_account_id, provider_ref, amount, currency, status,
				beneficiary_enc, beneficiary_label, created_at, updated_at, idempotency_key)
				VALUES (?, 'o', ?, ?, 'pa', ?, 1, 'INR', 'pending', ?, 'x', 1, 1, ?)`,
			"refunds": `INSERT INTO refunds (id, org_id, mode, app_id, payment_id, attempt_id, amount, currency, status, provider_ref,
				initiated_by, created_at, updated_at, idempotency_key)
				VALUES (?, 'o', ?, ?, 'p', 'a', 1, 'INR', 'pending', ?, 'api', 1, 1, ?)`,
		}
		for table, q := range ins {
			n := 0
			add := func(mode, app string, key any) error {
				n++
				id := table + "_" + string(rune('a'+n))
				args := []any{id, mode, app, "ref" + id, key}
				if table == "payouts" {
					args = []any{id, mode, app, "ref" + id, []byte("b"), key}
				}
				return db.Write(ctx, func(tx store.Tx) error { _, err := tx.ExecContext(ctx, q, args...); return err })
			}
			if err := add("test", "app_1", "k"); err != nil {
				t.Fatalf("%s: first insert: %v", table, err)
			}
			if err := add("test", "app_1", "k"); err == nil || !strings.Contains(err.Error(), "idempotency_key") && !store.IsUniqueViolation(err) {
				t.Errorf("%s: the same key twice was accepted: %v", table, err)
			}
			for _, c := range []struct{ mode, app string }{{"live", "app_1"}, {"test", "app_2"}} {
				if err := add(c.mode, c.app, "k"); err != nil {
					t.Errorf("%s: the key in %v: %v", table, c, err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := add("test", "app_1", nil); err != nil {
					t.Errorf("%s: keyless rows collided: %v", table, err)
				}
			}
		}
	})
}
