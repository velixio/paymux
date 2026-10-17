package jobs_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/jobs"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestNextRunEvery(t *testing.T) {
	s := jobs.Schedule{Name: "e", Kind: "k", Every: 90 * time.Second}
	got := jobs.NextRun(s, t0)
	if want := t0.Add(90 * time.Second); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextRunDaily(t *testing.T) {
	ist := mustLoc(t, "Asia/Kolkata")
	s := jobs.Schedule{Name: "d", Kind: "k", DailyAt: "03:00", Location: ist}
	cases := []struct{ after, want time.Time }{
		{time.Date(2026, 10, 17, 2, 59, 0, 0, ist), time.Date(2026, 10, 17, 3, 0, 0, 0, ist)},
		{time.Date(2026, 10, 17, 3, 0, 0, 0, ist), time.Date(2026, 10, 18, 3, 0, 0, 0, ist)},
		{time.Date(2026, 10, 17, 3, 0, 0, 1, ist), time.Date(2026, 10, 18, 3, 0, 0, 0, ist)},
	}
	for _, c := range cases {
		if got := jobs.NextRun(s, c.after); !got.Equal(c.want) {
			t.Errorf("after %v: got %v want %v", c.after, got, c.want)
		}
	}
}

func TestNextRunDailyDST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")

	// Spring forward: 02:30 does not exist on 2026-03-08; the run moves to 03:30 EDT.
	s := jobs.Schedule{Name: "d", Kind: "k", DailyAt: "02:30", Location: ny}
	after := time.Date(2026, 3, 7, 3, 0, 0, 0, ny)
	want := time.Date(2026, 3, 8, 3, 30, 0, 0, ny)
	got := jobs.NextRun(s, after)
	if !got.Equal(want) {
		t.Fatalf("spring forward: got %v want %v", got.In(ny), want)
	}
	if name, _ := got.In(ny).Zone(); name != "EDT" {
		t.Fatalf("zone %s", name)
	}

	// Fall back: 01:30 occurs twice on 2026-11-01; run once (first, EDT), then 11-02 EST.
	s = jobs.Schedule{Name: "d", Kind: "k", DailyAt: "01:30", Location: ny}
	first := jobs.NextRun(s, time.Date(2026, 11, 1, 0, 0, 0, 0, ny))
	if want := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC); !first.Equal(want) {
		t.Fatalf("first: got %v want %v", first.UTC(), want)
	}
	if name, _ := first.In(ny).Zone(); name != "EDT" {
		t.Fatalf("zone %s", name)
	}
	next := jobs.NextRun(s, first)
	if want := time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next: got %v want %v (11-02 01:30 EST)", next.UTC(), want)
	}
}

func TestAddScheduleValidation(t *testing.T) {
	db, c := newMigrated(t)
	q := newQueue(db, c)
	ist := mustLoc(t, "Asia/Kolkata")
	bad := map[string]jobs.Schedule{
		"both":      {Name: "a", Kind: "k", Every: time.Minute, DailyAt: "03:00", Location: ist},
		"neither":   {Name: "b", Kind: "k"},
		"hour 25":   {Name: "c", Kind: "k", DailyAt: "25:00", Location: ist},
		"minute 60": {Name: "d", Kind: "k", DailyAt: "03:60", Location: ist},
		"garbage":   {Name: "e", Kind: "k", DailyAt: "3am", Location: ist},
		"nil loc":   {Name: "f", Kind: "k", DailyAt: "03:00"},
		"no name":   {Kind: "k", Every: time.Minute},
		"no kind":   {Name: "g", Every: time.Minute},
		"neg every": {Name: "h", Kind: "k", Every: -time.Second},
	}
	for name, s := range bad {
		if err := q.AddSchedule(s); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if err := q.AddSchedule(jobs.Schedule{Name: "ok", Kind: "k", DailyAt: "03:00", Location: ist}); err != nil {
		t.Fatal(err)
	}
	if err := q.AddSchedule(jobs.Schedule{Name: "ok", Kind: "k", Every: time.Minute}); err == nil {
		t.Error("duplicate name: expected error")
	}
}
