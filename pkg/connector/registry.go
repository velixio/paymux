package connector

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type entry struct {
	factory Factory
	info    Info
}

var (
	regMu    sync.RWMutex
	registry = map[string]entry{}
)

// Register adds a connector to the global registry. It is meant to be called
// from init(). Registering an empty name, a nil factory or a duplicate name
// panics.
func Register(name string, f Factory, info Info) {
	if name == "" || f == nil {
		panic("connector: Register needs a name and a factory")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("connector: %q registered twice", name))
	}
	info.Name = name
	registry[name] = entry{f, info}
}

// Lookup returns the factory and static Info for a registered connector.
func Lookup(name string) (Factory, Info, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	e, ok := registry[name]
	return e.factory, e.info, ok
}

// List returns the Info of every registered connector, sorted by name.
func List() []Info {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Info, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResetRegistryForTest empties the registry. Tests only.
func ResetRegistryForTest() {
	regMu.Lock()
	defer regMu.Unlock()
	registry = map[string]entry{}
}

// MemKV is an in-memory KV for tests.
type MemKV struct {
	mu    sync.Mutex
	now   func() time.Time
	items map[string]memItem
}

type memItem struct {
	val     []byte
	expires time.Time // zero = never
}

// NewMemKV returns an empty KV; now may be nil for the wall clock.
func NewMemKV(now func() time.Time) *MemKV {
	if now == nil {
		now = time.Now
	}
	return &MemKV{now: now, items: map[string]memItem{}}
}

// Get implements KV.
func (m *MemKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	if !it.expires.IsZero() && !m.now().Before(it.expires) {
		delete(m.items, key)
		return nil, false, nil
	}
	return append([]byte(nil), it.val...), true, nil
}

// Set implements KV.
func (m *MemKV) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	it := memItem{val: append([]byte(nil), val...)}
	if ttl > 0 {
		it.expires = m.now().Add(ttl)
	}
	m.items[key] = it
	return nil
}

// Delete implements KV.
func (m *MemKV) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}
