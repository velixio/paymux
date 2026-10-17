package httpapi

import (
	"math"
	"sync"
	"time"
)

// DefaultRatePerSecond is the per-key request rate (spec section 12).
const DefaultRatePerSecond = 20

// limiter is an in-memory token bucket per API key. A bucket holds up to
// burst tokens and refills at rate tokens per second; a request takes one.
// Buckets live in one process: with several instances each allows the rate,
// which is acceptable for abuse protection.
type limiter struct {
	rate, burst float64
	now         func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int, now func() time.Time) *limiter {
	if rate == 0 {
		rate = DefaultRatePerSecond
	}
	if now == nil {
		now = time.Now
	}
	b := float64(burst)
	if burst <= 0 {
		b = math.Max(1, math.Ceil(rate))
	}
	return &limiter{rate: rate, burst: b, now: now, buckets: map[string]*bucket{}}
}

// allow takes a token for key. When none is left it returns how long until
// one is.
func (l *limiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 4096 {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// prune drops buckets that have been full for a while (they carry no state).
func (l *limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, k)
		}
	}
}
