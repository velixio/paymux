package hub

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/redact"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

// The purge job reads the retention settings, trims the logs by level and age,
// and is a scheduled job that exactly one instance runs at a time.
func TestLogPurgeJobAppliesTheSettings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		set := settings.New(db)
		if err := db.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Paymux", time.Now()) }); err != nil {
			t.Fatal(err)
		}
		if err := set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
			t.Fatal(err)
		}
		ldb, err := db.OpenLogs(ctx, migrations.FS)
		if err != nil {
			t.Fatal(err)
		}
		if ldb != db {
			t.Cleanup(func() { ldb.Close() })
		}
		now := time.Now()
		old := func(id, level string, ageDays int) store.LogRow {
			return store.LogRow{ID: id, At: now.AddDate(0, 0, -ageDays), Level: level, Kind: "app", Component: "t", Msg: id}
		}
		if err := ldb.Write(ctx, func(tx store.Tx) error {
			return store.InsertLogs(ctx, tx, []store.LogRow{old("w_old", "warn", 8), old("w_new", "warn", 1), old("e_old", "error", 100), old("e_new", "error", 30)})
		}); err != nil {
			t.Fatal(err)
		}
		rec := dblog.New(dblog.Options{DB: ldb, Redactor: redact.New()})
		l := &dbLogs{rec: rec, red: redact.New(), db: ldb, shared: ldb == db}
		q := jobs.New(db, jobs.Options{Logger: slog.New(slog.DiscardHandler)})
		if err := l.registerPurge(q, db, set); err != nil {
			t.Fatal(err)
		}
		if err := db.Write(ctx, func(tx store.Tx) error { return q.Enqueue(ctx, tx, jobs.Job{Kind: logPurgeKind}) }); err != nil {
			t.Fatal(err)
		}
		if n, err := q.RunOnce(ctx); err != nil || n < 1 {
			t.Fatalf("ran %d: %v", n, err)
		}
		var left []string
		_ = ldb.Read(ctx, func(q store.Querier) error {
			rows, err := store.SearchLogs(ctx, q, store.LogFilter{})
			for _, r := range rows {
				left = append(left, r.ID)
			}
			return err
		})
		if len(left) != 2 {
			t.Errorf("left %v, want w_new and e_new", left)
		}
	})
}
