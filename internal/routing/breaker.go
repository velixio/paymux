package routing

import (
	"sync"
	"time"
)

// Breaker parameters (spec §8): 5 consecutive failures within 5 minutes open
// the breaker for 2 minutes.
const (
	BreakerFailures = 5
	BreakerWindow   = 5 * time.Minute
	BreakerCooldown = 2 * time.Minute
)

type breakerState struct {
	streak    []time.Time
	openUntil time.Time
}

// Breaker tracks CreateCheckout failures per provider account. Safe for concurrent use.
type Breaker struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*breakerState
}

// NewBreaker returns a breaker using clock now (nil means time.Now).
func NewBreaker(now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{now: now, state: map[string]*breakerState{}}
}

func (b *Breaker) get(id string) *breakerState {
	s, ok := b.state[id]
	if !ok {
		s = &breakerState{}
		b.state[id] = s
	}
	return s
}

// Failure records a failed call.
func (b *Breaker) Failure(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	s := b.get(id)
	s.streak = append(s.streak, now)
	for len(s.streak) > 0 && now.Sub(s.streak[0]) > BreakerWindow {
		s.streak = s.streak[1:]
	}
	if len(s.streak) >= BreakerFailures {
		s.openUntil = now.Add(BreakerCooldown)
		s.streak = nil
	}
}

// Success resets the consecutive-failure streak.
func (b *Breaker) Success(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.get(id).streak = nil
}

// Open reports whether the account's breaker is open now.
func (b *Breaker) Open(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.now().Before(b.get(id).openUntil)
}
