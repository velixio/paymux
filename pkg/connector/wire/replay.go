package wire

import (
	"container/list"
	"sync"
	"time"
)

// DefaultReplayCacheSize is the number of message ids a handler remembers.
const DefaultReplayCacheSize = 10000

// replayCache remembers message ids for as long as a signed message could
// still pass the timestamp check (the tolerance applies either way, so twice
// Tolerance), bounded by size: when full the oldest id is forgotten first.
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

// firstSeen records id and reports true, or reports false when it has been seen
// within the window.
func (c *replayCache) firstSeen(id string, now time.Time) bool {
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
		return false
	}
	if c.order.Len() >= c.max {
		oldest := c.order.Front()
		delete(c.ids, oldest.Value.(entry).id)
		c.order.Remove(oldest)
	}
	c.ids[id] = c.order.PushBack(entry{id, now.Add(c.ttl)})
	return true
}
