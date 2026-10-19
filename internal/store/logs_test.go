package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// logsDB opens the log store of a migrated main database: a separate file on
// SQLite, the same database on PostgreSQL.
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

func writeLogs(t *testing.T, l *store.DB, rows ...store.LogRow) {
	t.Helper()
	if err := l.Write(context.Background(), func(tx store.Tx) error { return store.InsertLogs(context.Background(), tx, rows) }); err != nil {
		t.Fatal(err)
	}
}

func searchLogs(t *testing.T, l *store.DB, f store.LogFilter) []store.LogRow {
	t.Helper()
	var out []store.LogRow
	if err := l.Read(context.Background(), func(q store.Querier) (err error) { out, err = store.SearchLogs(context.Background(), q, f); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

var logT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func row(id string, at time.Duration, level, kind, msg string) store.LogRow {
	return store.LogRow{ID: id, At: logT0.Add(at), Level: level, Kind: kind, Component: "test", Msg: msg}
}

func TestLogsSeparateFileOnSQLiteSharedOnPostgres(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		if db.Dialect().Name() == "sqlite" {
			if l == db {
				t.Fatal("SQLite logs must live in their own database")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(db.Path()), "logs.db")); err != nil {
				t.Errorf("logs.db: %v", err)
			}
		} else if l != db {
			t.Error("PostgreSQL logs share the main database")
		}
	})
}

func TestLogsRoundTripAndFilters(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		full := store.LogRow{ID: "log_1", At: logT0, Level: "error", Kind: "provider", Component: "connector", Msg: "payu 504",
			RequestID: "req_1", JobID: "job_1", OrgID: "org_1", AppID: "app_1", PaymentID: "pay_1", AttemptID: "att_1", RefundID: "rfd_1",
			SubscriptionID: "sub_1", ProviderAccountID: "pa_1", Connector: "payu", IDs: `{"mandate_id":"mdt_9","provider_order_id":"ord_77"}`,
			Attrs: `{"k":"v"}`, Request: `{"method":"POST"}`, Response: `{"body":"gateway timeout"}`, Status: 504, DurationMS: 1500}
		writeLogs(t, l, full,
			row("log_2", time.Minute, "warn", "http", "bad signature"),
			row("log_3", 2*time.Minute, "error", "app", "something Broke_100%"),
			row("log_4", 3*time.Minute, "warn", "job", "retry"))
		got, err := func() (store.LogRow, error) {
			var r store.LogRow
			err := l.Read(context.Background(), func(q store.Querier) (err error) { r, err = store.GetLog(context.Background(), q, "log_1"); return })
			return r, err
		}()
		if err != nil || got != full {
			t.Fatalf("round trip: %+v %v", got, err)
		}
		for name, tc := range map[string]struct {
			f    store.LogFilter
			want []string
		}{
			"all, newest first": {store.LogFilter{}, []string{"log_4", "log_3", "log_2", "log_1"}},
			"level":             {store.LogFilter{Level: "warn"}, []string{"log_4", "log_2"}},
			"kind":              {store.LogFilter{Kind: "provider"}, []string{"log_1"}},
			"connector":         {store.LogFilter{Connector: "payu"}, []string{"log_1"}},
			"account":           {store.LogFilter{ProviderAccountID: "pa_1"}, []string{"log_1"}},
			"request":           {store.LogFilter{RequestID: "req_1"}, []string{"log_1"}},
			"payment":           {store.LogFilter{PaymentID: "pay_1"}, []string{"log_1"}},
			"any id: column":    {store.LogFilter{AnyID: "rfd_1"}, []string{"log_1"}},
			"any id: json":      {store.LogFilter{AnyID: "mdt_9"}, []string{"log_1"}},
			"any id: none":      {store.LogFilter{AnyID: "mdt_"}, nil},
			"from":              {store.LogFilter{From: logT0.Add(90 * time.Second)}, []string{"log_4", "log_3"}},
			"to":                {store.LogFilter{To: logT0.Add(90 * time.Second)}, []string{"log_2", "log_1"}},
			"text in msg":       {store.LogFilter{Text: "504"}, []string{"log_1"}},
			"text is lowercase": {store.LogFilter{Text: "SIGNATURE"}, []string{"log_2"}},
			"text in response":  {store.LogFilter{Text: "gateway"}, []string{"log_1"}},
			"text % and _":      {store.LogFilter{Text: "broke_100%"}, []string{"log_3"}},
			"text wildcard":     {store.LogFilter{Text: "%"}, []string{"log_3"}},
			"limit":             {store.LogFilter{Limit: 2}, []string{"log_4", "log_3"}},
			"combined":          {store.LogFilter{Level: "error", From: logT0.Add(30 * time.Second)}, []string{"log_3"}},
		} {
			rows := searchLogs(t, l, tc.f)
			var ids []string
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(tc.want) {
				t.Errorf("%s: got %v want %v", name, ids, tc.want)
			}
		}
	})
}

func TestLogsListOmitsBodiesUnlessAsked(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		r := row("log_1", 0, "error", "provider", "x")
		r.Request, r.Response = "REQ", "RESP"
		writeLogs(t, l, r)
		if got := searchLogs(t, l, store.LogFilter{})[0]; got.Request != "" || got.Response != "" {
			t.Errorf("list rows carry bodies: %+v", got)
		}
		if got := searchLogs(t, l, store.LogFilter{WithBodies: true})[0]; got.Request != "REQ" || got.Response != "RESP" {
			t.Errorf("export rows lack bodies: %+v", got)
		}
	})
}

func TestLogsKeysetPagination(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		var rows []store.LogRow
		for i := 0; i < 7; i++ {
			// Two rows share a millisecond: the id breaks the tie.
			rows = append(rows, row(fmt.Sprintf("log_%02d", i), time.Duration(i/2)*time.Second, "warn", "app", "m"))
		}
		writeLogs(t, l, rows...)
		var seen []string
		f := store.LogFilter{Limit: 3}
		for page := 0; page < 5; page++ {
			got := searchLogs(t, l, f)
			if len(got) == 0 {
				break
			}
			for _, r := range got {
				seen = append(seen, r.ID)
			}
			last := got[len(got)-1]
			f.BeforeAt, f.BeforeID = last.At, last.ID
		}
		want := []string{"log_06", "log_05", "log_04", "log_03", "log_02", "log_01", "log_00"}
		if fmt.Sprint(seen) != fmt.Sprint(want) {
			t.Errorf("pages = %v want %v", seen, want)
		}
	})
}

func TestPurgeLogsByLevelInBatches(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		var rows []store.LogRow
		for i := 0; i < 5; i++ {
			rows = append(rows, row(fmt.Sprintf("w%d", i), time.Duration(i)*time.Hour, "warn", "app", "w"))
			rows = append(rows, row(fmt.Sprintf("e%d", i), time.Duration(i)*time.Hour, "error", "app", "e"))
		}
		writeLogs(t, l, rows...)
		cutoff := logT0.Add(150 * time.Minute) // rows 0, 1, 2 are older
		total := 0
		for {
			var n int
			if err := l.Write(context.Background(), func(tx store.Tx) (err error) {
				n, err = store.PurgeLogs(context.Background(), tx, "warn", cutoff, 2)
				return
			}); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			if n > 2 {
				t.Fatalf("batch of %d", n)
			}
			total += n
		}
		if total != 3 {
			t.Errorf("purged %d warnings", total)
		}
		if got := len(searchLogs(t, l, store.LogFilter{Level: "error"})); got != 5 {
			t.Errorf("errors must be untouched: %d", got)
		}
		if got := len(searchLogs(t, l, store.LogFilter{Level: "warn"})); got != 2 {
			t.Errorf("warnings left: %d", got)
		}
	})
}

func TestTrimLogsKeepsTheNewest(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		var rows []store.LogRow
		for i := 0; i < 10; i++ {
			rows = append(rows, row(fmt.Sprintf("log_%02d", i), time.Duration(i)*time.Second, "warn", "app", "m"))
		}
		writeLogs(t, l, rows...)
		var n int
		if err := l.Write(context.Background(), func(tx store.Tx) (err error) { n, err = store.TrimLogs(context.Background(), tx, 4, 100); return }); err != nil {
			t.Fatal(err)
		}
		if n != 6 {
			t.Errorf("trimmed %d", n)
		}
		got := searchLogs(t, l, store.LogFilter{})
		if len(got) != 4 || got[0].ID != "log_09" || got[3].ID != "log_06" {
			t.Errorf("kept %+v", got)
		}
		var c int
		_ = l.Read(context.Background(), func(q store.Querier) (err error) { c, err = store.CountLogs(context.Background(), q); return })
		if c != 4 {
			t.Errorf("count = %d", c)
		}
	})
}

func TestLogsDatabaseIsRecreatedWhenCorrupt(t *testing.T) {
	db := storetest.SQLite(t)
	if err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(db.Path()), "logs.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database at all, just garbage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := logsDB(t, db)
	writeLogs(t, l, row("log_1", 0, "warn", "app", "after recovery"))
	if got := searchLogs(t, l, store.LogFilter{}); len(got) != 1 {
		t.Errorf("%+v", got)
	}
}

// A log write never waits for the main database's write lock.
func TestLogWritesDoNotTakeTheMainWriteLock(t *testing.T) {
	db := storetest.SQLite(t)
	if err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatal(err)
	}
	l := logsDB(t, db)
	hold, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = db.Write(context.Background(), func(tx store.Tx) error {
			close(hold)
			<-release
			return nil
		})
	}()
	<-hold
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- l.Write(context.Background(), func(tx store.Tx) error {
			return store.InsertLogs(context.Background(), tx, []store.LogRow{row("log_1", 0, "error", "app", "m")})
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a log write waited for the main database's write transaction")
	}
}
