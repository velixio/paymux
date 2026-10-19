package dashboard

import (
	"sync"
	"time"
)

// limiter counts failures per key inside a sliding window, in memory. A key is
// locked once it has max failures within the window.
type limiter struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	fails map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

// recent drops expired entries of key and returns what is left. Caller holds mu.
func (l *limiter) recent(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = ts
	return ts
}

func (l *limiter) locked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, now)) >= l.max
}

func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Bound memory against a flood of distinct keys: sweep stale keys once the
	// map gets large.
	if len(l.fails) > 10_000 {
		for k := range l.fails {
			l.recent(k, now)
		}
	}
	// Still huge after the sweep: an attack with endless distinct keys. Forget an
	// arbitrary key rather than grow without bound.
	if len(l.fails) > 100_000 {
		for k := range l.fails {
			delete(l.fails, k)
			break
		}
	}
	l.fails[key] = append(l.recent(key, now), now)
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

func (l *limiter) size() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.fails) }
