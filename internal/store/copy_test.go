package store_test

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

func migratedDB(t *testing.T, db *store.DB) *store.DB {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return db
}

var binary = []byte{0, 1, 2, 0xff, 0xfe, 0, 'a', 0, 0x80}

// populate fills a migrated database with rows that stress the copy: binary
// data with zero bytes, unicode, NULLs, the largest integers, JSON text.
func populate(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux ₹", t0); err != nil {
			return err
		}
		if err := store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "Café 日本語", Slug: "cafe", CreatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertPayment(ctx, tx, store.Payment{ID: "pay_1", OrgID: store.DefaultOrgID, AppID: "app_1", Mode: "live", Kind: "one_time",
			Amount: math.MaxInt64 - 1, Currency: "INR", Status: "created", Metadata: map[string]string{"k": "v \"quoted\" ✓"}, ClientSecretHash: "x",
			ExpiresAt: t0, Version: 1, Flags: []string{"a"}, CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertPayment(ctx, tx, store.Payment{ID: "pay_2", OrgID: store.DefaultOrgID, AppID: "app_1", Mode: "test", Kind: "one_time",
			Amount: 1, Currency: "USD", Status: "created", ClientSecretHash: "y", ExpiresAt: t0, Version: 1, CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertBlob(ctx, tx, store.Blob{ID: "blob_1", OrgID: store.DefaultOrgID, Kind: "invoice_pdf", ContentType: "application/pdf",
			SHA256: "abc", Data: binary, CreatedAt: t0}); err != nil {
			return err
		}
		if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "live", Connector: "x",
			Name: "n", Status: "active", ConfigEnc: binary, CreatedAt: t0}); err != nil {
			return err
		}
		_, err := store.ReserveIdempotencyKey(ctx, tx, "app_1", "live", "k1", "hash", t0, 24*time.Hour, time.Minute)
		if err != nil {
			return err
		}
		return store.CompleteIdempotencyKey(ctx, tx, "app_1", "live", "k1", 201, binary)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertCopied(t *testing.T, dst *store.DB) {
	t.Helper()
	_ = dst.Read(ctx, func(q store.Querier) error {
		var name string
		var amount int64
		if err := q.QueryRowContext(ctx, "SELECT a.name, p.amount FROM apps a JOIN payments p ON p.app_id = a.id WHERE p.id = 'pay_1'").Scan(&name, &amount); err != nil ||
			name != "Café 日本語" || amount != math.MaxInt64-1 {
			t.Errorf("app/payment: %q %d %v", name, amount, err)
		}
		var data []byte
		if err := q.QueryRowContext(ctx, "SELECT data FROM blobs WHERE id = 'blob_1'").Scan(&data); err != nil || !bytes.Equal(data, binary) {
			t.Errorf("blob %v %v", data, err)
		}
		var enc []byte
		if err := q.QueryRowContext(ctx, "SELECT config_enc FROM provider_accounts WHERE id = 'pa_1'").Scan(&enc); err != nil || !bytes.Equal(enc, binary) {
			t.Errorf("config_enc %v %v", enc, err)
		}
		var resp []byte
		if err := q.QueryRowContext(ctx, "SELECT response_enc FROM idempotency_keys WHERE key = 'k1'").Scan(&resp); err != nil || !bytes.Equal(resp, binary) {
			t.Errorf("response_enc %v %v", resp, err)
		}
		var meta string
		if err := q.QueryRowContext(ctx, "SELECT metadata FROM payments WHERE id = 'pay_1'").Scan(&meta); err != nil || !strings.Contains(meta, "✓") {
			t.Errorf("metadata %q %v", meta, err)
		}
		var nulls int
		if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM payments WHERE order_ref IS NULL AND customer_id IS NULL").Scan(&nulls); err != nil || nulls != 2 {
			t.Errorf("NULLs not preserved: %d %v", nulls, err)
		}
		return nil
	})
}

func TestCopyAllAcrossDialects(t *testing.T) {
	type pair struct {
		name     string
		src, dst func(t *testing.T) *store.DB
	}
	sqlite := func(t *testing.T) *store.DB { return migratedDB(t, storetest.SQLite(t)) }
	postgres := func(t *testing.T) *store.DB { return migratedDB(t, storetest.Postgres(t)) }
	for _, p := range []pair{{"sqlite to sqlite", sqlite, sqlite}, {"sqlite to postgres", sqlite, postgres},
		{"postgres to sqlite", postgres, sqlite}, {"postgres to postgres", postgres, postgres}} {
		t.Run(p.name, func(t *testing.T) {
			src, dst := p.src(t), p.dst(t)
			populate(t, src)
			var progress []string
			rep, err := store.CopyAll(ctx, src, dst, store.CopyOptions{BatchSize: 1, Progress: func(table string, rows int64) { progress = append(progress, table) }})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Tables) < 10 || rep.Tables[0].Table != "orgs" || len(progress) != len(rep.Tables) || progress[0] != "orgs" {
				t.Fatalf("report: %d tables, first %q, progress %v", len(rep.Tables), rep.Tables[0].Table, progress)
			}
			seen := map[string]store.TableReport{}
			for _, tr := range rep.Tables {
				seen[tr.Table] = tr
				if tr.SourceChecksum != tr.TargetChecksum {
					t.Errorf("%s: checksums differ %s / %s", tr.Table, tr.SourceChecksum, tr.TargetChecksum)
				}
			}
			if seen["payments"].Rows != 2 || seen["blobs"].Rows != 1 || seen["orgs"].Rows != 1 || seen["jobs"].Rows != 0 || rep.Rows < 7 {
				t.Errorf("row counts: payments=%d blobs=%d orgs=%d total=%d", seen["payments"].Rows, seen["blobs"].Rows, seen["orgs"].Rows, rep.Rows)
			}
			assertCopied(t, dst)
		})
	}
}

func TestCopyAllRefusesANonEmptyTargetUnlessForced(t *testing.T) {
	src, dst := migratedDB(t, storetest.SQLite(t)), migratedDB(t, storetest.SQLite(t))
	populate(t, src)
	err := dst.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Other", t0) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CopyAll(ctx, src, dst, store.CopyOptions{}); !errors.Is(err, store.ErrTargetNotEmpty) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("non-empty target: %v", err)
	}
	var name string
	_ = dst.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, "SELECT name FROM orgs").Scan(&name) })
	if name != "Other" {
		t.Errorf("a refused copy touched the target: %q", name)
	}
	if _, err := store.CopyAll(ctx, src, dst, store.CopyOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	_ = dst.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, "SELECT name FROM orgs").Scan(&name) })
	if name != "Paymux ₹" {
		t.Errorf("forced copy: %q", name)
	}
	assertCopied(t, dst)
}

func TestCopyAllRefusesMismatchedSchemas(t *testing.T) {
	src := migratedDB(t, storetest.SQLite(t))
	unmigrated := storetest.SQLite(t)
	if _, err := store.CopyAll(ctx, src, unmigrated, store.CopyOptions{}); err == nil || !strings.Contains(err.Error(), "schemas differ") {
		t.Errorf("unmigrated target: %v", err)
	}
	if _, err := store.CopyAll(ctx, src, src, store.CopyOptions{}); err == nil {
		t.Error("copying a database onto itself")
	}
}

func TestBackupSQLite(t *testing.T) {
	db := migratedDB(t, storetest.SQLite(t))
	populate(t, db)
	dest := filepath.Join(t.TempDir(), "sub", "backup.db")
	if err := db.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("backup file: %v %v", st, err)
	}
	// The file is a complete, openable database.
	copyDB, err := store.Open(ctx, "sqlite://"+dest, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	assertCopied(t, copyDB)
	if err := db.Backup(ctx, dest); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("overwriting a backup: %v", err)
	}
	// Writes after the backup do not reach it.
	_ = db.Write(ctx, func(tx store.Tx) error {
		return store.InsertApp(ctx, tx, store.App{ID: "app_2", OrgID: store.DefaultOrgID, Name: "n", Slug: "later", CreatedAt: t0})
	})
	var n int
	_ = copyDB.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM apps").Scan(&n) })
	if n != 1 {
		t.Errorf("backup is not a snapshot: %d apps", n)
	}
}

func TestBackupPostgresPointsToPgDump(t *testing.T) {
	db := storetest.Postgres(t)
	err := db.Backup(ctx, filepath.Join(t.TempDir(), "x.db"))
	if !errors.Is(err, store.ErrBackupUnsupported) || !strings.Contains(err.Error(), "pg_dump") {
		t.Errorf("err = %v", err)
	}
}
