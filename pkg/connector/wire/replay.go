package wire

import (
	"container/list"
	"sync"
	"time"
)

// DefaultReplayCacheSize is the number of message ids a handler remembers. An
// id is kept for twice Tolerance (10 minutes), so this serves about 166 signed
// requests a second; above that the handler answers 503 rather than forget an id
// early (see replayCache).
const DefaultReplayCacheSize = 100000

// replayCache remembers message ids for as long as a signed message could
// still pass the timestamp check (the tolerance applies either way, so twice
// Tolerance), bounded by size. It never evicts an id that is still inside its
// window: when it is full a new id is refused (firstSeen reports full) and the
// caller answers 503, so a replay inside the window is always refused and the
// memory stays bounded.
//
// It is per process. A remote service running several instances behind a load
// balancer needs a shared store (or sticky routing) to reject a replay sent to
// another instance; the timestamp tolerance still bounds the window.
type replayCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	order *list.List // of entry, oldest first
	ids   map[string]*list.Element
}

type entry struct {
	id      string
	expires time.Time
}

func newReplayCache(max int, ttl time.Duration) *replayCache {
	if max <= 0 {
		max = DefaultReplayCacheSize
	}
	return &replayCache{max: max, ttl: ttl, order: list.New(), ids: map[string]*list.Element{}}
}

// firstSeen records id and reports fresh=true. It reports fresh=false when id
// has been seen within the window, and full=true (nothing recorded) when the
// cache holds its maximum of ids that are all still inside their window.
func (c *replayCache) firstSeen(id string, now time.Time) (fresh, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.order.Front(); e != nil; e = c.order.Front() {
		if now.Before(e.Value.(entry).expires) {
			break
		}
		delete(c.ids, e.Value.(entry).id)
		c.order.Remove(e)
	}
	if _, dup := c.ids[id]; dup {
		return false, false
	}
	if c.order.Len() >= c.max {
		return false, true
	}
	c.ids[id] = c.order.PushBack(entry{id, now.Add(c.ttl)})
	return true, false
}
