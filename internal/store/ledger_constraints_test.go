package store_test

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// H-13. The ledger's invariants hold in the database, not only in the code that
// writes it.

func exec(t *testing.T, db *store.DB, q string, args ...any) error {
	t.Helper()
	ctx := context.Background()
	return db.Write(ctx, func(tx store.Tx) error { _, err := tx.ExecContext(ctx, q, args...); return err })
}

const (
	insPayment = `INSERT INTO payments (id, org_id, app_id, mode, kind, amount, currency, amount_received, amount_refunded, status,
		client_secret_hash, expires_at, created_at, updated_at) VALUES (?, 'o', 'a', 'test', 'one_time', ?, 'INR', 0, ?, 'created', 'h', 1, 1, 1)`
	insRefund = `INSERT INTO refunds (id, org_id, app_id, mode, payment_id, attempt_id, amount, currency, status, provider_ref, initiated_by,
		created_at, updated_at) VALUES (?, 'o', 'a', 'test', 'p', 'att', ?, 'INR', 'pending', ?, 'api', 1, 1)`
	insPayout = `INSERT INTO payouts (id, org_id, mode, app_id, provider_account_id, provider_ref, amount, currency, status,
		beneficiary_enc, beneficiary_label, created_at, updated_at) VALUES (?, 'o', 'test', 'a', 'pa', ?, ?, 'INR', 'pending', ?, 'x', 1, 1)`
	insLedger = `INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, payment_id, refund_id, dispute_id, occurred_at)
		VALUES (?, 'o', 'test', ?, ?, 'INR', 'p', ?, ?, 1)`
)

func TestMoneyAmountsMustBePositive(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		for _, amount := range []int64{0, -5} {
			if err := exec(t, db, insPayment, "pay_bad", amount, 0); err == nil {
				t.Errorf("payment of %d accepted", amount)
			}
			if err := exec(t, db, insRefund, "ref_bad", amount, "r_bad"); err == nil {
				t.Errorf("refund of %d accepted", amount)
			}
			if err := exec(t, db, insPayout, "po_bad", "r_bad", amount, []byte("b")); err == nil {
				t.Errorf("payout of %d accepted", amount)
			}
		}
		if err := exec(t, db, insPayment, "pay_neg", 100, -1); err == nil {
			t.Error("a payment with a negative amount_refunded was accepted")
		}
		// A refund the provider made on its own can exceed what was received; the
		// truth is recorded (flag refund_exceeds_balance), so there is no upper bound.
		if err := exec(t, db, insPayment, "pay_ok", 100, 250); err != nil {
			t.Errorf("a payment refunded beyond its amount was refused: %v", err)
		}
		if err := exec(t, db, insRefund, "ref_ok", 1, "r_ok"); err != nil {
			t.Errorf("a refund of 1: %v", err)
		}
		if err := exec(t, db, insPayout, "po_ok", "r_ok", 1, []byte("b")); err != nil {
			t.Errorf("a payout of 1: %v", err)
		}
	})
}

func TestLedgerRefusesTheSameEntryTwice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		if err := exec(t, db, insLedger, "led_1", "refund", -100, "ref_1", nil); err != nil {
			t.Fatal(err)
		}
		if err := exec(t, db, insLedger, "led_2", "refund", -100, "ref_1", nil); err == nil {
			t.Error("a refund was booked twice")
		}
		if err := exec(t, db, insLedger, "led_3", "refund", -100, "ref_2", nil); err != nil {
			t.Errorf("another refund: %v", err)
		}
		for i, kind := range []string{"dispute_release", "dispute_loss"} {
			id := "led_d" + string(rune('a'+i))
			if err := exec(t, db, insLedger, id, kind, 1, nil, "dsp_1"); err != nil {
				t.Fatal(err)
			}
			if err := exec(t, db, insLedger, id+"2", kind, 1, nil, "dsp_1"); err == nil {
				t.Errorf("%s booked twice for one dispute", kind)
			}
		}
		// A capped hold can be topped up when the dispute is lost (A-13).
		for _, id := range []string{"led_h1", "led_h2"} {
			if err := exec(t, db, insLedger, id, "dispute_hold", -1, nil, "dsp_1"); err != nil {
				t.Errorf("hold %s: %v", id, err)
			}
		}
	})
}

func TestLedgerEntriesAreNeverChanged(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		if err := exec(t, db, insLedger, "led_1", "charge", 100, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := exec(t, db, "UPDATE ledger_entries SET amount = 1 WHERE id = 'led_1'"); err == nil {
			t.Error("a ledger entry was updated")
		}
		if err := exec(t, db, "DELETE FROM ledger_entries WHERE id = 'led_1'"); err == nil {
			t.Error("a ledger entry was deleted")
		}
		var amount int64
		_ = db.Read(context.Background(), func(q store.Querier) error {
			return q.QueryRowContext(context.Background(), "SELECT amount FROM ledger_entries WHERE id = 'led_1'").Scan(&amount)
		})
		if amount != 100 {
			t.Errorf("amount = %d", amount)
		}
	})
}

// migrate0033 returns the database migrated up to just before the H-13
// migration, and a func that runs it.
func beforeLedgerConstraints(t *testing.T, db *store.DB) func() error {
	t.Helper()
	ctx := context.Background()
	before := fstest.MapFS{}
	for _, dir := range []string{"sqlite", "postgres"} {
		entries, _ := fs.ReadDir(migrations.FS, dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "0033_") {
				continue
			}
			b, _ := fs.ReadFile(migrations.FS, dir+"/"+e.Name())
			before[dir+"/"+e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := db.Migrate(ctx, before); err != nil {
		t.Fatal(err)
	}
	return func() error { return db.Migrate(ctx, migrations.FS) }
}

// The migration refuses data that already breaks an invariant, and changes
// nothing, rather than rewriting it.
func TestLedgerConstraintsMigrationRefusesViolatingData(t *testing.T) {
	for name, setup := range map[string][]string{
		"payment of zero":        {`INSERT INTO payments (id, org_id, app_id, mode, kind, amount, currency, status, client_secret_hash, expires_at, created_at, updated_at) VALUES ('p0', 'o', 'a', 'test', 'one_time', 0, 'INR', 'created', 'h', 1, 1, 1)`},
		"negative refunded":      {`INSERT INTO payments (id, org_id, app_id, mode, kind, amount, amount_refunded, currency, status, client_secret_hash, expires_at, created_at, updated_at) VALUES ('p0', 'o', 'a', 'test', 'one_time', 5, -1, 'INR', 'created', 'h', 1, 1, 1)`},
		"refund of zero":         {`INSERT INTO refunds (id, org_id, app_id, mode, payment_id, attempt_id, amount, currency, status, provider_ref, initiated_by, created_at, updated_at) VALUES ('r0', 'o', 'a', 'test', 'p', 'att', 0, 'INR', 'pending', 'x', 'api', 1, 1)`},
		"refund booked twice":    {`INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, refund_id, occurred_at) VALUES ('l1', 'o', 'test', 'refund', -1, 'INR', 'r1', 1)`, `INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, refund_id, occurred_at) VALUES ('l2', 'o', 'test', 'refund', -1, 'INR', 'r1', 1)`},
		"dispute released twice": {`INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, dispute_id, occurred_at) VALUES ('l1', 'o', 'test', 'dispute_release', 1, 'INR', 'd1', 1)`, `INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, dispute_id, occurred_at) VALUES ('l2', 'o', 'test', 'dispute_release', 1, 'INR', 'd1', 1)`},
	} {
		storetest.Each(t, func(t *testing.T, db *store.DB) {
			migrate := beforeLedgerConstraints(t, db)
			for _, q := range setup {
				if err := exec(t, db, q); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
			}
			if err := migrate(); err == nil {
				t.Errorf("%s: the migration accepted it", name)
			}
		})
	}
}

// Entries from before the attempt was recorded are matched where that is
// unambiguous: by the instant the attempt was captured, by the refund's attempt,
// or by the payment's only captured attempt. Otherwise they stay without one.
func TestLedgerConstraintsMigrationBackfillsTheAttempt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrate := beforeLedgerConstraints(t, db)
		att := func(id, pay string, updated int64) string {
			return `INSERT INTO payment_attempts (id, org_id, mode, payment_id, provider_account_id, provider_ref, status, amount_received, created_at, updated_at)
				VALUES ('` + id + `', 'o', 'test', '` + pay + `', 'pa', 'ref_` + id + `', 'captured', 500, 1, ` + itoa(updated) + `)`
		}
		led := func(id, kind, pay string, refund any, at int64) string {
			r := "NULL"
			if refund != nil {
				r = "'" + refund.(string) + "'"
			}
			return `INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, payment_id, refund_id, occurred_at)
				VALUES ('` + id + `', 'o', 'test', '` + kind + `', 500, 'INR', '` + pay + `', ` + r + `, ` + itoa(at) + `)`
		}
		for _, q := range []string{
			att("a_only", "p1", 99), led("l_only", "charge", "p1", nil, 7), // one captured attempt, other instant
			att("a_sale", "p2", 10), att("a_dup", "p2", 20), led("l_sale", "charge", "p2", nil, 10), led("l_dup", "charge", "p2", nil, 20),
			att("a_x", "p3", 30), att("a_y", "p3", 40), led("l_amb", "charge", "p3", nil, 5), // two attempts, no instant matches
			`INSERT INTO refunds (id, org_id, app_id, mode, payment_id, attempt_id, amount, currency, status, provider_ref, initiated_by, created_at, updated_at)
				VALUES ('r1', 'o', 'a', 'test', 'p2', 'a_dup', 500, 'INR', 'succeeded', 'rr', 'api', 1, 1)`,
			led("l_ref", "refund", "p2", "r1", 30),
		} {
			if err := exec(t, db, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := migrate(); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string]string{"l_only": "a_only", "l_sale": "a_sale", "l_dup": "a_dup", "l_amb": "", "l_ref": "a_dup"} {
			var got string
			_ = db.Read(context.Background(), func(q store.Querier) error {
				return q.QueryRowContext(context.Background(), "SELECT COALESCE(attempt_id, '') FROM ledger_entries WHERE id = '"+id+"'").Scan(&got)
			})
			if got != want {
				t.Errorf("%s: attempt %q, want %q", id, got, want)
			}
		}
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
