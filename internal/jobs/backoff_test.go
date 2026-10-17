package jobs

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0, 5 * time.Second},
		{2, 0, 10 * time.Second},
		{3, 0, 20 * time.Second},
		{20, 0, time.Hour},
		{1, 0.2, 6 * time.Second},
		{1, -0.2, 4 * time.Second},
	}
	for _, c := range cases {
		if got := Backoff(c.attempt, c.jitter); got != c.want {
			t.Errorf("Backoff(%d,%v)=%v want %v", c.attempt, c.jitter, got, c.want)
		}
	}
}
