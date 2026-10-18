package dblog_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func logsDB(t *testing.T, db *store.DB) *store.DB {
	t.Helper()
	l, err := db.OpenLogs(context.Background(), migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if l != db {
		t.Cleanup(func() { l.Close() })
	}
	return l
}

func days(n int) time.Time { return now.AddDate(0, 0, -n) }

func seed(t *testing.T, l *store.DB, rows ...store.LogRow) {
	t.Helper()
	if err := l.Write(context.Background(), func(tx store.Tx) error { return store.InsertLogs(context.Background(), tx, rows) }); err != nil {
		t.Fatal(err)
	}
}

func logRow(id string, at time.Time, level string) store.LogRow {
	return store.LogRow{ID: id, At: at, Level: level, Kind: "app", Component: "t", Msg: id}
}

func remaining(t *testing.T, l *store.DB) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	_ = l.Read(context.Background(), func(q store.Querier) error {
		rows, err := store.SearchLogs(context.Background(), q, store.LogFilter{Limit: 10000})
		for _, r := range rows {
			got[r.ID] = true
		}
		return err
	})
	return got
}

func retention(m map[string]int) func(context.Context, string) (int, error) {
	return func(_ context.Context, key string) (int, error) {
		v, ok := m[key]
		if !ok {
			return 0, fmt.Errorf("no setting %s", key)
		}
		return v, nil
	}
}

func TestPurgeKeepsErrorsLongerThanWarnings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		seed(t, l,
			logRow("w_old", days(8), "warn"), logRow("w_new", days(6), "warn"),
			logRow("e_old", days(91), "error"), logRow("e_mid", days(60), "error"), logRow("e_new", days(1), "error"),
			logRow("i_old", days(8), "info"), logRow("i_new", days(2), "info"))
		p := dblog.NewPurger(dblog.PurgeOptions{Logs: l, Main: db, Now: func() time.Time { return now }, Batch: 2,
			Retention: retention(map[string]int{"log_retention_error_days": 90, "log_retention_warn_days": 7, "log_max_rows": 1000, "audit_retention_days": 0})})
		res, err := p.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got := remaining(t, l)
		for _, gone := range []string{"w_old", "e_old", "i_old"} {
			if got[gone] {
				t.Errorf("%s should have been purged", gone)
			}
		}
		for _, kept := range []string{"w_new", "e_mid", "e_new", "i_new"} {
			if !got[kept] {
				t.Errorf("%s should have been kept", kept)
			}
		}
		if res.Warnings != 2 || res.Errors != 1 || res.Trimmed != 0 {
			t.Errorf("%+v", res)
		}
	})
}

func TestPurgeCapsTheTableOldestFirst(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		var rows []store.LogRow
		for i := 0; i < 20; i++ {
			rows = append(rows, logRow(fmt.Sprintf("r%02d", i), now.Add(-time.Duration(20-i)*time.Minute), "error"))
		}
		seed(t, l, rows...)
		p := dblog.NewPurger(dblog.PurgeOptions{Logs: l, Main: db, Now: func() time.Time { return now }, Batch: 3,
			Retention: retention(map[string]int{"log_retention_error_days": 90, "log_retention_warn_days": 7, "log_max_rows": 8, "audit_retention_days": 0})})
		res, err := p.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got := remaining(t, l)
		if len(got) != 8 || !got["r19"] || !got["r12"] || got["r11"] || res.Trimmed != 12 {
			t.Errorf("kept %d (%v), result %+v", len(got), got, res)
		}
	})
}

func TestPurgeAuditRespectsRetentionAndNeverBelowAYear(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		ctx := context.Background()
		if err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SeedOrg(ctx, tx, "Paymux", now); err != nil {
				return err
			}
			for id, age := range map[string]int{"aud_ancient": 800, "aud_old": 401, "aud_edge": 399, "aud_recent": 10} {
				if err := store.InsertAudit(ctx, tx, store.AuditRow{ID: id, OrgID: store.DefaultOrgID, ActorType: "system", Action: "x.y", At: days(age)}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		count := func() int {
			var n int
			_ = db.Read(ctx, func(q store.Querier) error {
				return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log WHERE action = 'x.y'").Scan(&n)
			})
			return n
		}
		mk := func(auditDays int) *dblog.Purger {
			return dblog.NewPurger(dblog.PurgeOptions{Logs: l, Main: db, Now: func() time.Time { return now }, OrgID: store.DefaultOrgID,
				Retention: retention(map[string]int{"log_retention_error_days": 90, "log_retention_warn_days": 7, "log_max_rows": 100000, "audit_retention_days": auditDays})})
		}
		if _, err := mk(0).Run(ctx); err != nil || count() != 4 {
			t.Fatalf("0 keeps everything: %d %v", count(), err)
		}
		// A value below 365 days is never honoured, even if the setting were bypassed.
		if _, err := mk(30).Run(ctx); err != nil || count() != 4 {
			t.Fatalf("30 days must not purge audit: %d %v", count(), err)
		}
		res, err := mk(400).Run(ctx)
		if err != nil || count() != 2 || res.Audit != 2 {
			t.Fatalf("400 days: %d %+v %v", count(), res, err)
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_log WHERE action = 'audit.purge'").Scan(&n)
		})
		if n != 1 {
			t.Errorf("the purge itself is audited: %d", n)
		}
		_ = audit.System
	})
}
