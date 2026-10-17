package authz

import (
	"context"
	"sync"
	"time"
)

// Cache is a Resolver over a loader (the roles table) with a short TTL. A
// change made through this process calls Invalidate; other processes pick it
// up within the TTL. When the loader fails the last good data is kept; with
// none, every custom role is denied.
type Cache struct {
	load func(context.Context) (map[Role][]Action, error)
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	roles   map[Role][]Action
	fetched time.Time
	valid   bool
}

// NewCache returns a Cache; now may be nil for the wall clock.
func NewCache(load func(context.Context) (map[Role][]Action, error), ttl time.Duration, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{load: load, ttl: ttl, now: now}
}

// Actions implements Resolver.
func (c *Cache) Actions(role Role) ([]Action, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.now().Sub(c.fetched) >= c.ttl {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		roles, err := c.load(ctx)
		cancel()
		if err == nil {
			c.roles, c.valid = roles, true
		}
		// On error keep serving the old data (if any) and retry on the next call.
		if err == nil || !c.valid {
			c.fetched = c.now()
		}
		if err != nil && c.valid {
			c.fetched = c.now().Add(-c.ttl + time.Second) // retry soon
		}
	}
	a, ok := c.roles[role]
	return a, ok
}

// Invalidate makes the next lookup reload.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}
