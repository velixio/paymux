package dblog

import (
	"strconv"
	"sync"
	"time"
)

// A request that is rejected costs the sender almost nothing and used to cost
// a database row. The throttle bounds those rows: a source gets so many rows
// per route and status per window, and a route and status gets so many rows
// per window from all sources together. What was skipped is counted, and the
// count is carried by the next row the source gets.

const (
	throttleWindow = time.Minute
	// throttleKeys bounds the memory of the throttle; past it, sources share
	// one bucket per route and status.
	throttleKeys = 4096

	rejectedPerSource = 1  // 4xx on /v1 and /hooks: one row per source, route and status
	rejectedGlobal    = 30 // and at most this many per route and status, whoever sent them
	errorPerSource    = 5  // 5xx: the hub's own trouble, so a few more
	errorGlobal       = 20
)

type window struct {
	start      time.Time
	count      int
	suppressed int
}

type throttle struct {
	mu     sync.Mutex
	source map[string]*window // source|route|status
	global map[string]*window // route|status
}

func newThrottle() *throttle {
	return &throttle{source: map[string]*window{}, global: map[string]*window{}}
}

// roll starts a new window when the old one is over and returns what it had
// suppressed.
func (w *window) roll(now time.Time) (carried int) {
	if w.start.IsZero() || now.Sub(w.start) >= throttleWindow {
		carried = w.suppressed
		*w = window{start: now}
	}
	return carried
}

// allow says whether a row may be written for this event. carried is the
// number of rows skipped since the last one that was written.
func (t *throttle) allow(now time.Time, source, route string, status int) (ok bool, carried int) {
	perSource, global := rejectedPerSource, rejectedGlobal
	if status >= 500 {
		perSource, global = errorPerSource, errorGlobal
	}
	rs := route + "|" + strconv.Itoa(status)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.source) >= throttleKeys {
		t.sweep(now)
		if len(t.source) >= throttleKeys {
			source = "*" // too many distinct sources: they share a bucket
		}
	}
	sk := source + "|" + rs
	sw := t.source[sk]
	if sw == nil {
		sw = &window{}
		t.source[sk] = sw
	}
	gw := t.global[rs]
	if gw == nil {
		gw = &window{}
		t.global[rs] = gw
	}
	carried = sw.roll(now) + gw.roll(now)
	switch {
	case sw.count >= perSource:
		sw.suppressed++
		return false, 0
	case gw.count >= global:
		gw.suppressed++
		return false, 0
	}
	sw.count++
	gw.count++
	return true, carried
}

// sweep forgets sources whose window is over (and that skipped nothing, so no
// count is lost).
func (t *throttle) sweep(now time.Time) {
	for k, w := range t.source {
		if now.Sub(w.start) >= throttleWindow && w.suppressed == 0 {
			delete(t.source, k)
		}
	}
	if len(t.source) >= throttleKeys { // everything is busy: drop the oldest half
		for k, w := range t.source {
			if now.Sub(w.start) >= throttleWindow/2 {
				delete(t.source, k)
			}
		}
	}
}
