package dblog_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func countRows(t *testing.T, l *store.DB) int {
	t.Helper()
	var n int
	if err := l.Read(context.Background(), func(q store.Querier) (err error) { n, err = store.CountLogs(context.Background(), q); return }); err != nil {
		t.Fatal(err)
	}
	return n
}

// Anyone who can reach /hooks or /v1 can make the hub answer 4xx as fast as the
// network allows. Each of those answers used to become a row.
func TestRejectedRequestsFromOneSourceAreRateLimited(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		clk := &fakeClock{t: now}
		e := newEnv(t, db, func(o *dblog.Options) { o.Now = clk.Now })
		ctx := context.Background()
		ev := dblog.HTTPEvent{Method: "POST", Route: "POST /hooks/{provider_account_id}", Status: 401, Source: "203.0.113.9"}
		for range 10_000 {
			e.rec.HTTP(ctx, ev)
		}
		rows := e.rows(t)
		if len(rows) != 1 {
			t.Fatalf("%d rows for 10,000 rejected requests from one source", len(rows))
		}
		if got := e.rec.Stats().Suppressed; got != 9_999 {
			t.Errorf("suppressed = %d", got)
		}
		// A minute later the source is heard again, and the row says how many were skipped.
		clk.Add(61 * time.Second)
		e.rec.HTTP(ctx, ev)
		rows = e.rows(t)
		if len(rows) != 2 {
			t.Fatalf("%d rows after the window", len(rows))
		}
		var carried bool
		for _, r := range rows {
			if strings.Contains(r.Attrs, `"suppressed":9999`) {
				carried = true
			}
		}
		if !carried {
			t.Errorf("no row carries the count of skipped requests: %+v", rows)
		}
		// Another kind of rejection from the same source is its own key.
		e.rec.HTTP(ctx, dblog.HTTPEvent{Method: "POST", Route: "POST /v1/payments", Status: 401, Source: "203.0.113.9"})
		if n := len(e.rows(t)); n != 3 {
			t.Errorf("a different route was suppressed: %d rows", n)
		}
	})
}

// Many sources cannot multiply the rows without bound either.
func TestRejectedRequestsFromManySourcesAreCappedAsAWhole(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		clk := &fakeClock{t: now}
		e := newEnv(t, db, func(o *dblog.Options) { o.Now = clk.Now })
		ctx := context.Background()
		for i := range 5_000 {
			e.rec.HTTP(ctx, dblog.HTTPEvent{Method: "POST", Route: "POST /hooks/{provider_account_id}", Status: 400,
				Source: fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256)})
		}
		if n := len(e.rows(t)); n > 100 {
			t.Errorf("%d rows from 5,000 sources in a minute", n)
		}
		if n := len(e.rows(t)); n == 0 {
			t.Error("nothing recorded")
		}
	})
}

// Server errors are the hub's own trouble and are kept, but a loop that fails
// the same way ten thousand times is one problem.
func TestRepeatedServerErrorsAreBoundedToo(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		clk := &fakeClock{t: now}
		e := newEnv(t, db, func(o *dblog.Options) { o.Now = clk.Now })
		for range 10_000 {
			e.rec.HTTP(context.Background(), dblog.HTTPEvent{Method: "GET", Route: "GET /pay/{id}", Status: 500, Source: "203.0.113.9"})
		}
		if n := len(e.rows(t)); n < 1 || n > 50 {
			t.Errorf("%d rows for 10,000 identical 500s", n)
		}
	})
}

// The row cap is applied as rows arrive, not only by the hourly job.
func TestTheRowCapIsEnforcedWhileRowsArrive(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		p := dblog.NewPurger(dblog.PurgeOptions{Logs: l, Main: db, Now: func() time.Time { return now },
			Retention: retention(map[string]int{"log_retention_error_days": 90, "log_retention_warn_days": 7, "log_max_rows": 1000, "log_max_mb": 1024, "audit_retention_days": 0})})
		e := newEnv(t, db, func(o *dblog.Options) {
			o.TrimEvery = 200
			o.Trim = func(ctx context.Context) error { _, err := p.Trim(ctx); return err }
		})
		for i := range 5_000 {
			e.log.Error(fmt.Sprintf("failure %d", i))
		}
		e.rec.Flush(context.Background())
		if n := countRows(t, e.db); n > 1_000+200+100 {
			t.Errorf("%d rows kept with a cap of 1000", n)
		}
	})
}

// The cap in megabytes holds as well as the cap in rows.
func TestTheSizeCapTrimsTheOldestRows(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		l := logsDB(t, db)
		big := strings.Repeat("x", 100<<10)
		var rows []store.LogRow
		for i := range 60 { // about 6 MB
			r := logRow(fmt.Sprintf("log_%03d", i), now.Add(time.Duration(i)*time.Second), "error")
			r.Response = big
			rows = append(rows, r)
		}
		seed(t, l, rows...)
		p := dblog.NewPurger(dblog.PurgeOptions{Logs: l, Main: db, Now: func() time.Time { return now.Add(time.Hour) },
			Retention: retention(map[string]int{"log_retention_error_days": 90, "log_retention_warn_days": 7, "log_max_rows": 100000, "log_max_mb": 2, "audit_retention_days": 0})})
		n, err := p.Trim(context.Background())
		if err != nil || n == 0 {
			t.Fatalf("trimmed %d: %v", n, err)
		}
		left := remaining(t, l)
		if len(left) > 21 || len(left) < 5 {
			t.Errorf("%d rows left of 60 under a 2 MB cap", len(left))
		}
		if !left["log_059"] || left["log_000"] {
			t.Errorf("the oldest rows should go first: %v", left)
		}
	})
}
