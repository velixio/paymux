package billing

import (
	"errors"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+1800)

func at(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, ist) }

func TestPeriodBoundaryMonthClamp(t *testing.T) {
	anchor := at(2026, 1, 31, 10, 0)
	want := []time.Time{at(2026, 1, 31, 10, 0), at(2026, 2, 28, 10, 0), at(2026, 3, 31, 10, 0), at(2026, 4, 30, 10, 0)}
	for k, w := range want {
		if got := Boundary(anchor, Month, 1, k); !got.Equal(w) {
			t.Fatalf("k=%d got %v want %v", k, got, w)
		}
	}
	if got := Boundary(at(2027, 1, 29, 0, 0), Month, 1, 1); !got.Equal(at(2027, 2, 28, 0, 0)) {
		t.Fatalf("non-leap Feb clamp: %v", got)
	}
	if got := Boundary(at(2028, 1, 31, 0, 0), Month, 1, 1); !got.Equal(at(2028, 2, 29, 0, 0)) {
		t.Fatalf("leap Feb clamp: %v", got)
	}
}

func TestPeriodBoundaryOtherIntervals(t *testing.T) {
	a := at(2026, 2, 29-1, 9, 0) // 2026-02-28
	cases := []struct {
		iv    Interval
		count int
		k     int
		want  time.Time
	}{
		{Day, 1, 3, at(2026, 3, 3, 9, 0)},
		{Week, 2, 1, at(2026, 3, 14, 9, 0)},
		{Month, 3, 1, at(2026, 5, 28, 9, 0)},
		{Year, 1, 1, at(2027, 2, 28, 9, 0)},
	}
	for _, c := range cases {
		if got := Boundary(a, c.iv, c.count, c.k); !got.Equal(c.want) {
			t.Errorf("%s x%d k=%d: got %v want %v", c.iv, c.count, c.k, got, c.want)
		}
	}
	if got := Boundary(at(2028, 2, 29, 0, 0), Year, 1, 1); !got.Equal(at(2029, 2, 28, 0, 0)) {
		t.Fatalf("leap-day yearly clamp: %v", got)
	}
}

func TestPeriodContaining(t *testing.T) {
	anchor := at(2026, 1, 31, 10, 0)
	start, end, k := PeriodContaining(anchor, Month, 1, at(2026, 3, 1, 0, 0))
	if k != 1 || !start.Equal(at(2026, 2, 28, 10, 0)) || !end.Equal(at(2026, 3, 31, 10, 0)) {
		t.Fatalf("got %v %v %d", start, end, k)
	}
}

func TestChargeTiming(t *testing.T) {
	charge := at(2026, 11, 1, 3, 0)
	if got := PreDebitAt(charge, 48*time.Hour); !got.Equal(at(2026, 10, 30, 3, 0)) {
		t.Fatalf("predebit %v", got)
	}
	notified := at(2026, 10, 31, 12, 0)
	if ok, next := ChargeAllowed(notified, at(2026, 11, 1, 3, 0)); ok || !next.Equal(at(2026, 11, 1, 12, 0)) {
		t.Fatalf("within 24h of notice must be refused, got ok=%v next=%v", ok, next)
	}
	if ok, _ := ChargeAllowed(notified, at(2026, 11, 1, 12, 0)); !ok {
		t.Fatal("exactly 24h after notice is allowed")
	}
}

func TestSnapUPIWindow(t *testing.T) {
	w := DefaultUPIPeakHours
	cases := map[time.Time]time.Time{
		at(2026, 11, 1, 9, 59):  at(2026, 11, 1, 9, 59),
		at(2026, 11, 1, 10, 0):  at(2026, 11, 1, 13, 0),
		at(2026, 11, 1, 12, 59): at(2026, 11, 1, 13, 0),
		at(2026, 11, 1, 17, 0):  at(2026, 11, 1, 21, 30),
		at(2026, 11, 1, 21, 30): at(2026, 11, 1, 21, 30),
		at(2026, 11, 1, 23, 0):  at(2026, 11, 1, 23, 0),
	}
	for in, want := range cases {
		if got := SnapOutsidePeak(in, w); !got.Equal(want) {
			t.Errorf("%v: got %v want %v", in.Format("15:04"), got.Format("15:04"), want.Format("15:04"))
		}
	}
	// UTC input is interpreted in IST: 04:30 UTC = 10:00 IST -> 13:00 IST.
	if got := SnapOutsidePeak(time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC), w); !got.Equal(at(2026, 11, 1, 13, 0)) {
		t.Fatalf("utc input: %v", got.In(ist))
	}
}

func TestDunning(t *testing.T) {
	def := DefaultDunning
	if len(def) != 3 || def[0] != 24*time.Hour || def[2] != 5*24*time.Hour {
		t.Fatalf("default dunning %v", def)
	}
	long := []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 5 * time.Hour}
	if got := RetrySchedule(long, MethodUPI); len(got) != 3 {
		t.Fatalf("UPI capped at 3 retries, got %d", len(got))
	}
	if got := RetrySchedule(long, MethodCard); len(got) != 5 {
		t.Fatalf("card keeps schedule, got %d", len(got))
	}
	if err := ValidateDunning([]time.Duration{30 * time.Minute}); err == nil {
		t.Fatal("retry under 1h accepted")
	}
	if err := ValidateDunning(make([]time.Duration, 7)); err == nil {
		t.Fatal("7 retries accepted")
	}
}

func TestMandateMax(t *testing.T) {
	const limit = 1_500_000 // ₹15,000
	if got, err := MandateMax(49900, 15000, limit); err != nil || got != 74850 {
		t.Fatalf("1.5x: %d %v", got, err)
	}
	if got, err := MandateMax(1_200_000, 15000, limit); err != nil || got != limit {
		t.Fatalf("capped at limit: %d %v", got, err)
	}
	if _, err := MandateMax(1_600_000, 15000, limit); !errors.Is(err, ErrMandateLimit) {
		t.Fatalf("over limit: %v", err)
	}
	if got, _ := MandateMax(1000, 10000, limit); got != 1000 {
		t.Fatalf("headroom 1.0x: %d", got)
	}
}
