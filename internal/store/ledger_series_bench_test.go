package store_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
)

// BenchmarkLedgerSeriesAllTime times the all-time daily series over one
// million ledger entries spread across five years (spec §6: under 300 ms).
func BenchmarkLedgerSeriesAllTime(b *testing.B) {
	bg := context.Background()
	db, err := store.Open(bg, "", b.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	if err := db.Migrate(bg, migrations.FS); err != nil {
		b.Fatal(err)
	}
	const n = 1_000_000
	start := time.Date(2021, 10, 25, 0, 0, 0, 0, time.UTC)
	span := 5 * 365 * 24 * time.Hour
	step := span.Milliseconds() / n
	err = db.Write(bg, func(tx store.Tx) error {
		if err := store.SeedOrg(bg, tx, "Paymux", start); err != nil {
			return err
		}
		if err := store.InsertApp(bg, tx, store.App{ID: "app_a", OrgID: store.DefaultOrgID, Name: "Alpha", Slug: "alpha", CreatedAt: start}); err != nil {
			return err
		}
		if err := store.InsertPayment(bg, tx, pay("pay_1", "app_a", "live", "succeeded", "", "", start)); err != nil {
			return err
		}
		_, err := tx.ExecContext(bg, `WITH RECURSIVE s(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM s WHERE i < ?)
			INSERT INTO ledger_entries (id, org_id, mode, kind, amount, currency, payment_id, occurred_at)
			SELECT 'led_' || i, ?, 'live', CASE WHEN i % 10 = 0 THEN 'refund' ELSE 'charge' END,
				CASE WHEN i % 10 = 0 THEN -100 ELSE 1000 END, CASE WHEN i % 7 = 0 THEN 'USD' ELSE 'INR' END, 'pay_1', ? + i * ?
			FROM s`, n-1, store.DefaultOrgID, start.UnixMilli(), step)
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	f := store.SeriesFilter{OrgID: store.DefaultOrgID, Mode: "live", From: start, To: start.Add(span + time.Hour)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := db.Read(bg, func(q store.Querier) error {
			rows, err := store.LedgerSeries(bg, q, f)
			if err == nil && len(rows) < 1000 {
				b.Fatalf("only %d slots", len(rows))
			}
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
