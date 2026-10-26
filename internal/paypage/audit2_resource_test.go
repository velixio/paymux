package paypage_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/core"
)

// Anyone who holds one return URL (every customer does) can make the hub call
// the provider's API once per request: a loop on /return/{pa}/{ref} spends the
// merchant's provider rate limit, which every other checkout on that account
// shares. Returns of one attempt that follow each other closely are answered
// from what is stored; the webhook and the poller still catch up.
func TestAudit2ReturnHammerIsNotOneProviderCallPerRequest(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{})
		_, path := e.startAndAttempt(c)
		var before int
		e.fake.Locked(func() { before = len(e.fake.Fetches) })
		const n = 40
		for range n {
			if r := e.do("GET", path, "", nil); r.status >= 500 {
				t.Fatalf("return: %d %s", r.status, r.body)
			}
		}
		var calls int
		e.fake.Locked(func() { calls = len(e.fake.Fetches) - before })
		if calls > 2 {
			t.Fatalf("%d returns of one attempt made %d provider fetches; want them throttled", n, calls)
		}
		// A return a little later asks the provider again.
		e.clk.Add(6 * time.Second)
		e.do("GET", path, "", nil)
		var after int
		e.fake.Locked(func() { after = len(e.fake.Fetches) - before })
		if after != calls+1 {
			t.Fatalf("a return after the interval made %d new fetches, want 1", after-calls)
		}
	})
}

// The same holds for ?fresh=1: each request made a new attempt row and a new
// checkout at the provider, without limit.
func TestAudit2FreshCheckoutHammerIsBounded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{})
		const n = 60
		for range n {
			if r := e.do("GET", c.path()+"&fresh=1", "", nil); r.status >= 500 {
				t.Fatalf("fresh: %d %s", r.status, r.body)
			}
		}
		if got := e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id); got >= n {
			t.Fatalf("%d fresh checkouts made %d attempts (and as many provider checkouts); want a cap per payment", n, got)
		}
	})
}
