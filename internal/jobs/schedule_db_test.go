package jobs_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func nextRunAt(t *testing.T, db *store.DB, name string) time.Time {
	t.Helper()
	var ms int64
	err := db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT next_run_at FROM schedules WHERE name = ?", name).Scan(&ms)
	})
	if err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(ms).UTC()
}

func TestScheduleEnqueuesOncePerDue(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, c := fixture(t, db)
		q.Register("tick", func(context.Context, json.RawMessage) error { return nil })
		if err := q.AddSchedule(jobs.Schedule{Name: "every-min", Kind: "tick", Every: time.Minute}); err != nil {
			t.Fatal(err)
		}
		runOnce(t, q) // first sighting records the row; nothing is due yet
		if n := len(getRows(t, db, "tick")); n != 0 {
			t.Fatalf("jobs at t0: %d", n)
		}
		if got := nextRunAt(t, db, "every-min"); !got.Equal(t0.Add(time.Minute)) {
			t.Fatalf("next=%v", got)
		}
		c.Add(time.Minute)
		runOnce(t, q)
		runOnce(t, q) // same instant: not due again
		if n := len(getRows(t, db, "tick")); n != 1 {
			t.Fatalf("jobs after first due: %d", n)
		}
		if r := one(t, db, "tick"); r.State != "done" {
			t.Fatalf("scheduled job should run in the same RunOnce: %+v", r)
		}
		c.Add(time.Minute)
		runOnce(t, q)
		if n := len(getRows(t, db, "tick")); n != 2 {
			t.Fatalf("jobs after second due: %d", n)
		}
	})
}

func TestScheduleCatchUpDoesNotBurst(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		q, c := fixture(t, db)
		q.Register("tick", func(context.Context, json.RawMessage) error { return nil })
		if err := q.AddSchedule(jobs.Schedule{Name: "m", Kind: "tick", Every: time.Minute}); err != nil {
			t.Fatal(err)
		}
		runOnce(t, q)
		c.Add(time.Hour) // process was down for an hour
		runOnce(t, q)
		runOnce(t, q)
		if n := len(getRows(t, db, "tick")); n != 1 {
			t.Fatalf("catch-up burst: %d jobs", n)
		}
		if got, want := nextRunAt(t, db, "m"), c.Now().Add(time.Minute); !got.Equal(want) {
			t.Fatalf("next=%v want %v", got, want)
		}
	})
}

func TestTwoInstancesScheduleOnce(t *testing.T) {
	db := storetest.Postgres(t)
	q1, c := fixture(t, db)
	q2 := newQueue(db, c)
	for _, q := range []*jobs.Queue{q1, q2} {
		if err := q.AddSchedule(jobs.Schedule{Name: "once", Kind: "tick", Every: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	runOnce(t, q1)
	c.Add(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		q := q1
		if i%2 == 1 {
			q = q2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.RunOnce(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(getRows(t, db, "tick")); n != 1 {
		t.Fatalf("jobs enqueued: %d, want exactly 1", n)
	}
}
