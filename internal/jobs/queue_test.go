package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

func newQueue(db *store.DB, c *clock) *jobs.Queue {
	return jobs.New(db, jobs.Options{
		Workers:      4,
		PollInterval: 10 * time.Millisecond,
		Lease:        5 * time.Minute,
		Clock:        c.Now,
		Jitter:       func() float64 { return 0 },
		Logger:       slog.New(slog.DiscardHandler),
	})
}

// fixture migrates db and returns a queue with a controllable clock.
func fixture(t *testing.T, db *store.DB) (*jobs.Queue, *clock) {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: t0}
	return newQueue(db, c), c
}

func enqueue(t *testing.T, db *store.DB, q *jobs.Queue, j jobs.Job) {
	t.Helper()
	if err := db.Write(ctx, func(tx store.Tx) error { return q.Enqueue(ctx, tx, j) }); err != nil {
		t.Fatal(err)
	}
}

type row struct {
	State, LastError string
	Attempts         int
	RunAt            time.Time
}

func getRows(t *testing.T, db *store.DB, kind string) []row {
	t.Helper()
	var out []row
	err := db.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT state, COALESCE(last_error, ''), attempts, run_at FROM jobs WHERE kind = ? ORDER BY created_at, id", kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			var ms int64
			if err := rows.Scan(&r.State, &r.LastError, &r.Attempts, &ms); err != nil {
				return err
			}
			r.RunAt = time.UnixMilli(ms).UTC()
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func one(t *testing.T, db *store.DB, kind string) row {
	t.Helper()
	r := getRows(t, db, kind)
	if len(r) != 1 {
		t.Fatalf("want 1 row for %s, got %d", kind, len(r))
	}
	return r[0]
}

func runOnce(t *testing.T, q *jobs.Queue) int {
	t.Helper()
	n, err := q.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJobRuns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		var got json.RawMessage
		q.Register("a.b", func(_ context.Context, p json.RawMessage) error { got = p; return nil })
		enqueue(t, db, q, jobs.Job{Kind: "a.b", Payload: map[string]any{"x": 1, "y": "z"}})
		if n := runOnce(t, q); n != 1 {
			t.Fatalf("ran %d", n)
		}
		if string(got) != `{"x":1,"y":"z"}` {
			t.Fatalf("payload %s", got)
		}
		if r := one(t, db, "a.b"); r.State != "done" {
			t.Fatalf("state %s", r.State)
		}
	})
}

func TestRolledBackEnqueueNeverRuns(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		var ran atomic.Int32
		q.Register("rb", func(context.Context, json.RawMessage) error { ran.Add(1); return nil })
		boom := errors.New("boom")
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := q.Enqueue(ctx, tx, jobs.Job{Kind: "rb", Payload: 1}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if n := runOnce(t, q); n != 0 || ran.Load() != 0 {
			t.Fatalf("n=%d ran=%d", n, ran.Load())
		}
		if len(getRows(t, db, "rb")) != 0 {
			t.Fatal("row survived rollback")
		}
	})
}

func TestRetryThenDead(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, c := fixture(t, db)
		q.Register("flaky", func(context.Context, json.RawMessage) error { return errors.New("nope") })
		enqueue(t, db, q, jobs.Job{Kind: "flaky", Payload: 1, MaxAttempts: 3})

		if n := runOnce(t, q); n != 1 {
			t.Fatalf("attempt1 ran %d", n)
		}
		r := one(t, db, "flaky")
		if r.State != "pending" || r.Attempts != 1 || !r.RunAt.Equal(t0.Add(5*time.Second)) || r.LastError != "nope" {
			t.Fatalf("after 1: %+v", r)
		}
		c.Add(4 * time.Second)
		if n := runOnce(t, q); n != 0 {
			t.Fatalf("ran too early: %d", n)
		}
		c.Add(1 * time.Second)
		if n := runOnce(t, q); n != 1 {
			t.Fatalf("attempt2 ran %d", n)
		}
		r = one(t, db, "flaky")
		if r.State != "pending" || r.Attempts != 2 || !r.RunAt.Equal(t0.Add(15*time.Second)) {
			t.Fatalf("after 2: %+v", r)
		}
		c.Add(10 * time.Second)
		if n := runOnce(t, q); n != 1 {
			t.Fatalf("attempt3 ran %d", n)
		}
		r = one(t, db, "flaky")
		if r.State != "dead" || r.Attempts != 3 || r.LastError != "nope" {
			t.Fatalf("after 3: %+v", r)
		}
		c.Add(24 * time.Hour)
		if n := runOnce(t, q); n != 0 {
			t.Fatalf("dead job ran: %d", n)
		}
	})
}

func TestPermanentError(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		q.Register("perm", func(context.Context, json.RawMessage) error {
			return jobs.Permanent(errors.New("bad input"))
		})
		enqueue(t, db, q, jobs.Job{Kind: "perm", Payload: 1})
		runOnce(t, q)
		r := one(t, db, "perm")
		if r.State != "dead" || r.Attempts != 1 || !strings.Contains(r.LastError, "bad input") {
			t.Fatalf("%+v", r)
		}
	})
}

func TestDedupe(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		q.Register("d", func(context.Context, json.RawMessage) error { return nil })
		j := jobs.Job{Kind: "d", Payload: 1, DedupeKey: "k1"}
		enqueue(t, db, q, j)
		enqueue(t, db, q, j) // no-op while pending
		if n := len(getRows(t, db, "d")); n != 1 {
			t.Fatalf("rows=%d", n)
		}
		runOnce(t, q)
		enqueue(t, db, q, j) // allowed again after done
		if n := len(getRows(t, db, "d")); n != 2 {
			t.Fatalf("rows=%d", n)
		}
	})
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, c := fixture(t, db)
		var ran atomic.Int32
		q.Register("lease", func(context.Context, json.RawMessage) error { ran.Add(1); return nil })
		enqueue(t, db, q, jobs.Job{Kind: "lease", Payload: 1})

		claimed, err := db.ClaimJobs(ctx, c.Now(), 5*time.Minute, 10) // "process killed" after claim
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claimed=%d err=%v", len(claimed), err)
		}
		if n := runOnce(t, q); n != 0 {
			t.Fatalf("leased job ran: %d", n)
		}
		c.Add(5 * time.Minute)
		if n := runOnce(t, q); n != 1 || ran.Load() != 1 {
			t.Fatalf("n=%d ran=%d", n, ran.Load())
		}
		r := one(t, db, "lease")
		if r.State != "done" || r.Attempts != 2 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestUnknownKindIsDead(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		enqueue(t, db, q, jobs.Job{Kind: "x.y", Payload: 1})
		runOnce(t, q)
		r := one(t, db, "x.y")
		if r.State != "dead" || r.LastError != `no handler registered for "x.y"` {
			t.Fatalf("%+v", r)
		}
	})
}

func TestHandlerPanicIsRetried(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, c := fixture(t, db)
		var calls atomic.Int32
		q.Register("pan", func(context.Context, json.RawMessage) error {
			if calls.Add(1) == 1 {
				panic("kaboom")
			}
			return nil
		})
		enqueue(t, db, q, jobs.Job{Kind: "pan", Payload: 1})
		runOnce(t, q)
		r := one(t, db, "pan")
		if r.State != "pending" || !strings.Contains(r.LastError, "kaboom") {
			t.Fatalf("%+v", r)
		}
		c.Add(5 * time.Second)
		runOnce(t, q)
		if r := one(t, db, "pan"); r.State != "done" || r.Attempts != 2 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestRegisterDuplicatePanics(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		h := func(context.Context, json.RawMessage) error { return nil }
		q.Register("dup", h)
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		q.Register("dup", h)
	})
}

func TestJobStats(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, c := fixture(t, db)
		enqueue(t, db, q, jobs.Job{Kind: "s", Payload: 1})
		c.Add(30 * time.Second)
		depth, oldest, err := db.JobStats(ctx, c.Now())
		if err != nil || depth["pending"] != 1 || oldest != 30*time.Second {
			t.Fatalf("depth=%v oldest=%v err=%v", depth, oldest, err)
		}
	})
}

func TestStartStopsOnCancel(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, _ := fixture(t, db)
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- q.Start(cctx) }()
		deadline := time.Now().Add(time.Second)
		for !q.Running() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if !q.Running() {
			t.Fatal("never started")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Start did not return within 1s")
		}
		if q.Running() {
			t.Fatal("still running")
		}
	})
}

func TestConcurrentClaimersRunEachJobOnce(t *testing.T) {
	db := storetest.Postgres(t)
	q1, c := fixture(t, db)
	q2 := newQueue(db, c)
	err := db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE ran (job_id TEXT NOT NULL, UNIQUE (job_id))")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h := func(_ context.Context, p json.RawMessage) error {
		calls.Add(1)
		var v struct{ N int }
		if err := json.Unmarshal(p, &v); err != nil {
			return jobs.Permanent(err)
		}
		return db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO ran (job_id) VALUES (?)", string(p))
			return err
		})
	}
	q1.Register("c", h)
	q2.Register("c", h)
	const total = 50
	for i := 0; i < total; i++ {
		enqueue(t, db, q1, jobs.Job{Kind: "c", Payload: map[string]int{"N": i}})
	}
	var wg sync.WaitGroup
	for _, q := range []*jobs.Queue{q1, q2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := q.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if calls.Load() != total {
		t.Fatalf("handler calls=%d want %d", calls.Load(), total)
	}
	var n int
	err = db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM ran").Scan(&n)
	})
	if err != nil || n != total {
		t.Fatalf("ran rows=%d err=%v", n, err)
	}
	for _, r := range getRows(t, db, "c") {
		if r.State != "done" || r.Attempts != 1 {
			t.Fatalf("%+v", r)
		}
	}
}
