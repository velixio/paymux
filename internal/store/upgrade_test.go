package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

// The upgrade guarantee (docs/upgrades.md): a database left by release N-1
// opens with release N, migrates forward without losing anything, and keeps
// working. The fixtures are frozen pictures of such a database, made at
// migration 0005 with rows in every table that existed then:
//
//	testdata/upgrade/v0005.sqlite        a SQLite database file
//	testdata/upgrade/v0005.postgres.sql  a pg_dump of a PostgreSQL database
//
// This test runs the current migrations over each and checks the data and a
// payment flow on the result.

// loadFixture returns a database holding the fixture, not yet migrated.
func loadFixture(t *testing.T, dialect string) *store.DB {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	switch dialect {
	case "sqlite":
		src, err := os.ReadFile(filepath.Join("testdata", "upgrade", "v0005.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "paymux.db")
		if err := os.WriteFile(path, src, 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(ctx, "sqlite://"+path, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	default:
		url := storetest.PostgresURL(t) // an empty schema of its own
		raw, err := sql.Open("pgx", url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, postgresDump(t)); err != nil {
			raw.Close()
			t.Fatalf("loading the PostgreSQL fixture: %v", err)
		}
		raw.Close()
		db, err := store.Open(ctx, url, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
}

// postgresDump is the pg_dump file made runnable inside the test's schema:
// psql meta-commands, session settings and the public. qualifier are dropped.
func postgresDump(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "upgrade", "v0005.postgres.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(l, `\`), strings.HasPrefix(l, "SET "), strings.HasPrefix(l, "SELECT pg_catalog.set_config"),
			strings.HasPrefix(l, "CREATE SCHEMA"):
			continue
		}
		out = append(out, strings.ReplaceAll(l, "public.", ""))
	}
	return strings.Join(out, "\n")
}

func eachFixture(t *testing.T, fn func(t *testing.T, db *store.DB, fresh func() *store.DB)) {
	t.Run("sqlite", func(t *testing.T) {
		fn(t, loadFixture(t, "sqlite"), func() *store.DB { return storetest.SQLite(t) })
	})
	t.Run("postgres", func(t *testing.T) {
		db := loadFixture(t, "postgres")
		fn(t, db, func() *store.DB { return storetest.Postgres(t) })
	})
}

func migrationFiles(t *testing.T) int {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestUpgradeFromTheBaselineKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	eachFixture(t, func(t *testing.T, db *store.DB, fresh func() *store.DB) {
		pending, err := db.PendingMigrations(ctx, migrations.FS)
		if err != nil {
			t.Fatal(err)
		}
		if want := migrationFiles(t) - fixtureVersion; pending != want || pending == 0 {
			t.Fatalf("the fixture should be %d migrations behind, pending=%d", want, pending)
		}
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if n, _ := db.PendingMigrations(ctx, migrations.FS); n != 0 {
			t.Fatalf("%d migrations still pending", n)
		}

		count := func(table string) (n int) {
			t.Helper()
			err := db.Read(ctx, func(q store.Querier) error {
				return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n)
			})
			if err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			return n
		}
		for table, want := range map[string]int{
			"orgs": 1, "apps": 1, "api_keys": 2, "customers": 1, "provider_accounts": 1, "app_provider_accounts": 1, "payments": 2,
			"payment_attempts": 2, "refunds": 1, "ledger_entries": 2, "org_settings": 2, "app_settings": 1, "audit_log": 1, "events": 1,
			"webhook_endpoints": 1, "webhook_deliveries": 1, "users": 1, "app_domains": 1, "connector_kv": 1, "jobs": 1, "schedules": 1,
			// Tables added since the baseline exist and start empty.
			"invoices": 0, "credit_notes": 0, "disputes": 0, "settlements": 0, "subscriptions": 0,
		} {
			if got := count(table); got != want {
				t.Errorf("%s has %d rows after the upgrade, want %d", table, got, want)
			}
		}

		err = db.Read(ctx, func(q store.Querier) error {
			p, err := store.GetPayment(ctx, q, "pay_01up1")
			if err != nil {
				return err
			}
			if p.Amount != 49900 || p.Currency != "INR" || p.Status != "succeeded" || p.AmountReceived != 49900 || p.AmountRefunded != 10000 ||
				p.CustomerID != "cus_01up" || p.Metadata["note"] != "café ₹499" || p.Metadata["plan"] != "pro" || !slices.Equal(p.Flags, []string{"refunded_partial"}) ||
				p.Version != 4 || p.OrderRef != "ord-1" || !p.ExpiresAt.Equal(time.UnixMilli(1760000100000)) {
				t.Errorf("payment after upgrade: %+v", p)
			}
			a, err := store.GetAttempt(ctx, q, "att_01up1")
			if err != nil {
				return err
			}
			if a.Status != "captured" || a.Method != "upi" || a.ProviderPaymentID != "pay_up1" || a.ProviderOrderID != "order_up1" || a.AmountReceived != 49900 ||
				a.NextAction != "" || !a.CheckoutExpiresAt.IsZero() {
				t.Errorf("attempt after upgrade: %+v", a)
			}
			if f, err := store.GetAttempt(ctx, q, "att_01up0"); err != nil || f.FailureCode != "card_declined" || f.FailureMessage != "The card was declined." {
				t.Errorf("failed attempt: %+v %v", f, err)
			}
			pa, err := store.GetProviderAccount(ctx, q, "pa_01up")
			if err != nil {
				return err
			}
			if !bytes.Equal(pa.ConfigEnc, []byte{0x00, 0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0xFF, 0x7F}) || pa.Connector != "razorpay" || pa.ProviderGSTIN != "29AAAAA0000A1Z5" {
				t.Errorf("provider account after upgrade: %+v", pa)
			}
			k, err := store.GetAPIKeyByPrefix(ctx, q, "pmx_test_sk_up01")
			if err != nil || k.Hash != "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae" || k.AppID != "app_01up" || !k.RevokedAt.IsZero() {
				t.Errorf("api key: %+v %v", k, err)
			}
			if k2, err := store.GetAPIKeyByPrefix(ctx, q, "pmx_live_sk_up02"); err != nil || k2.RevokedAt.IsZero() {
				t.Errorf("revoked key lost its revocation: %+v %v", k2, err)
			}
			r, err := store.GetRefund(ctx, q, "ref_01up1")
			if err != nil {
				return err
			}
			if r.Amount != 10000 || r.Status != "succeeded" || r.ProviderRefundID != "rfnd_up1" || r.Reason != "customer request" {
				t.Errorf("refund: %+v", r)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		// Settings written by the old release still resolve.
		set := settings.New(db)
		for app, want := range map[string]int{"app_01up": 45, "": 90} {
			var n int
			v, err := set.Resolve(ctx, app, "payment_ttl")
			if err != nil || v.Decode(&n) != nil || n != want {
				t.Errorf("payment_ttl for %q = %d (%v), want %d", app, n, err, want)
			}
		}

		// The upgraded schema is the schema a fresh install gets.
		want := fresh()
		if err := want.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		got, err := db.DescribeSchema(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := want.DescribeSchema(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, ref) {
			for name, tbl := range ref.Tables {
				if !reflect.DeepEqual(got.Tables[name], tbl) {
					t.Errorf("table %s differs after upgrade:\n upgraded %+v\n fresh    %+v", name, got.Tables[name], tbl)
				}
			}
			t.Error("the upgraded schema differs from a fresh install")
		}
	})
}

// A smoke flow on the upgraded database: new payments, a capture, a refund,
// and a refund of a payment that predates the upgrade.
func TestUpgradedDatabaseTakesPaymentsAndRefunds(t *testing.T) {
	ctx := context.Background()
	eachFixture(t, func(t *testing.T, db *store.DB, _ func() *store.DB) {
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		set := settings.New(db)
		if _, err := core.Bootstrap(ctx, db, set); err != nil {
			t.Fatalf("bootstrap on an existing install: %v", err)
		}
		svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: jobs.New(db, jobs.Options{})})
		sc := core.Scope{AppID: "app_01up", Mode: "test"}

		created, err := svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: sc, Amount: 2000, Currency: "INR", OrderRef: "after-upgrade",
			Customer: &core.CustomerInput{ExternalRef: "u1", Name: "Asha Rao", Email: "asha@example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		if created.Payment.CustomerID != "cus_01up" {
			t.Errorf("the existing customer should be reused: %q", created.Payment.CustomerID)
		}
		att, err := svc.CreateAttempt(ctx, created.Payment.ID, "pa_01up")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ApplyAttemptSnapshot(ctx, att.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 2000, Currency: "INR", ProviderPaymentID: "pay_after"}); err != nil {
			t.Fatal(err)
		}
		if p, err := svc.GetPaymentByID(ctx, created.Payment.ID); err != nil || p.Status != "succeeded" || p.AmountReceived != 2000 {
			t.Fatalf("new payment: %+v %v", p, err)
		}
		for _, id := range []string{created.Payment.ID, "pay_01up1"} {
			r, err := svc.CreateRefund(ctx, core.CreateRefundInput{Scope: sc, Actor: audit.System, InitiatedBy: "api", PaymentID: id, Amount: 500})
			if err != nil {
				t.Fatalf("refund of %s: %v", id, err)
			}
			if _, err := svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundSucceeded, ProviderRefundID: "rfnd_" + id}); err != nil {
				t.Fatal(err)
			}
		}
		if p, _ := svc.GetPaymentByID(ctx, "pay_01up1"); p.AmountRefunded != 10500 {
			t.Errorf("the old payment's refunded total: %d", p.AmountRefunded)
		}
		// The old attempt can be looked up the way webhooks do it.
		if err := db.Read(ctx, func(q store.Querier) error {
			a, err := store.FindAttempt(ctx, q, "pa_01up", "UPREF00000000000001", "", "")
			if err != nil || a.ID != "att_01up1" {
				t.Errorf("FindAttempt on an old row: %+v %v", a, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
