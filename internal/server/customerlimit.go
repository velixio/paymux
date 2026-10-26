package server

import (
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/clientip"
)

// customerBurst is how many requests one address may make at once on one
// route family before the steady rate applies.
const customerBurst = 20

// customerFamilies are the customer-facing path prefixes. Each is limited on
// its own, so a client using one family does not use up another's allowance.
var customerFamilies = []string{"/pay/", "/return/", "/l/", "/s/", "/i/"}

// CustomerLimit is a per-address token bucket for the customer routes, kept in
// memory. It caps what one client can do across many payments or links; the
// per-payment and per-link limits stay in their own handlers.
type CustomerLimit struct {
	perMinute int
	trusted   []netip.Prefix
	now       func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// NewCustomerLimit builds the limiter: perMinute requests a minute per
// address and route family, with a burst of 20. perMinute <= 0 disables it.
// trusted are the proxies whose X-Forwarded-For is believed.
func NewCustomerLimit(perMinute int, trusted []netip.Prefix) *CustomerLimit {
	return &CustomerLimit{perMinute: perMinute, trusted: trusted, now: time.Now, buckets: map[string]*bucket{}}
}

func familyOf(path string) string {
	for _, f := range customerFamilies {
		if strings.HasPrefix(path, f) {
			return f
		}
	}
	return ""
}

// Wrap limits the customer routes of next; every other path passes through.
func (l *CustomerLimit) Wrap(next http.Handler) http.Handler {
	if l == nil || l.perMinute <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fam := familyOf(r.URL.Path)
		if fam == "" {
			next.ServeHTTP(w, r)
			return
		}
		if wait, ok := l.take(fam+clientip.Group(clientip.From(r, l.trusted)), l.now()); !ok {
			secs := int(math.Ceil(wait.Seconds()))
			if secs < 1 {
				secs = 1
			}
			h := w.Header()
			h.Set("Retry-After", strconv.Itoa(secs))
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("Too many requests from your address. Please wait a moment and try again.\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// take spends one token of key's bucket, or says how long until there is one.
func (l *CustomerLimit) take(key string, now time.Time) (time.Duration, bool) {
	rate := float64(l.perMinute) / 60 // tokens a second
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now, rate)
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: customerBurst, at: now}
		l.buckets[key] = b
	}
	if el := now.Sub(b.at).Seconds(); el > 0 {
		b.tokens = math.Min(customerBurst, b.tokens+el*rate)
		b.at = now
	}
	if b.tokens < 1 {
		return time.Duration((1 - b.tokens) / rate * float64(time.Second)), false
	}
	b.tokens--
	return 0, true
}

// sweep drops buckets idle long enough to have refilled completely (they are
// the same as a new one), at most once a minute, and starts over rather than
// grow past a flood of distinct addresses.
func (l *CustomerLimit) sweep(now time.Time, rate float64) {
	if len(l.buckets) > 100_000 {
		l.buckets = map[string]*bucket{}
	}
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	idle := time.Duration(customerBurst / rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.at) >= idle {
			delete(l.buckets, k)
		}
	}
}

func (l *CustomerLimit) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
