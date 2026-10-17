package billing

import (
	"testing"
	"time"
)

func TestProrate(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC) // 30 days
	day := func(n float64) time.Time { return start.Add(time.Duration(n * 24 * float64(time.Hour))) }
	for name, tc := range map[string]struct {
		old, new int64
		at       time.Time
		want     int64
	}{
		"upgrade halfway":              {49900, 99900, day(15), 25000},
		"upgrade at the start":         {49900, 99900, start, 50000},
		"before the period":            {49900, 99900, start.Add(-time.Hour), 50000},
		"upgrade 10 days left":         {30000, 60000, day(20), 10000},
		"downgrade halfway":            {99900, 49900, day(15), -25000},
		"downgrade a third left":       {60000, 30000, day(20), -10000},
		"same amount":                  {49900, 49900, day(7), 0},
		"at the end":                   {49900, 99900, end, 0},
		"after the end":                {49900, 99900, end.Add(time.Hour), 0},
		"rounds up (half away from 0)": {0, 1, day(15), 1},  // 0.5 -> 1
		"rounds down":                  {0, 1, day(14), 1},  // 16/30 = 0.533 -> 1
		"rounds down small":            {0, 1, day(25), 0},  // 5/30 = 0.167 -> 0
		"negative half rounds away":    {1, 0, day(15), -1}, // -0.5 -> -1
		"negative small":               {1, 0, day(25), 0},  // -0.167 -> 0
		"huge amounts do not overflow": {1 << 50, 1 << 51, day(15), 1 << 49},
	} {
		if got := Prorate(tc.old, tc.new, start, end, tc.at); got != tc.want {
			t.Errorf("%s: got %d want %d", name, got, tc.want)
		}
	}
	if got := Prorate(1, 2, end, start, start); got != 0 {
		t.Errorf("an empty or inverted period prorates to %d", got)
	}
}

// Whatever the instant, upgrading and then downgrading back at the same
// moment nets to nothing, and the charge never exceeds the full difference.
func TestProrateIsSymmetricAndBounded(t *testing.T) {
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	for s := int64(0); s <= int64(end.Sub(start)/time.Second); s += 60 * 61 {
		at := start.Add(time.Duration(s) * time.Second)
		up, down := Prorate(10000, 25000, start, end, at), Prorate(25000, 10000, start, end, at)
		if up+down != 0 {
			t.Fatalf("at +%ds: up %d down %d", s, up, down)
		}
		if up < 0 || up > 15000 {
			t.Fatalf("at +%ds: %d outside [0, 15000]", s, up)
		}
	}
}
