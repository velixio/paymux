package checkout_test

import (
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// The customer pays at the provider, but the provider's own response handler
// fails: the customer never comes back to our return URL and no webhook is sent.
// Nothing in Paymux hears of the payment unless Paymux asks, so asking starts
// when the checkout starts (a form_post to a provider whose txnid is our
// reference has no provider order id to go by) and goes on until a grace period
// after the checkout expires.

// formPostCheckout is what PayU's connector returns: a form to post, and no
// provider order id.
func (e *env) formPostCheckout() {
	e.fake.Locked(func() {
		e.fake.CheckoutFn = func(req connector.CheckoutRequest) (connector.CheckoutResult, error) {
			return connector.CheckoutResult{NextAction: connector.NextAction{Kind: connector.ActionFormPost, URL: "https://secure.payu.example.com/_payment",
				Fields: map[string]string{"txnid": req.ProviderRef}}}, nil
		}
	})
}

func (e *env) fetches() (n int) {
	e.fake.Locked(func() { n = len(e.fake.Fetches) })
	return
}

// pollInfo reads the poll schedule of an attempt: when the next poll is due
// (zero when none is scheduled) and how many polls were made.
func (e *env) pollInfo(attemptID string) (next time.Time, count int) {
	e.t.Helper()
	var ms int64
	if err := e.db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT COALESCE(next_poll_at,0), poll_count FROM payment_attempts WHERE id = ?", attemptID).Scan(&ms, &count)
	}); err != nil {
		e.t.Fatal(err)
	}
	if ms != 0 {
		next = time.UnixMilli(ms).UTC()
	}
	return
}

func TestPollingStartsWithTheCheckoutAndFindsAPaymentWhoseReturnWasLost(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.formPostCheckout()
		p := e.payment2()
		st, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		next, count := e.pollInfo(st.Attempt.ID)
		if d := next.Sub(e.clk.Now()); d < time.Minute || d > 3*time.Minute || count != 0 {
			t.Fatalf("after the checkout starts the first poll should be about 2 minutes out: next=%v (in %v) count=%d", next, d, count)
		}
		// Not due yet: the provider is not asked.
		if err := e.flow.PollDue(ctx); err != nil || e.fetches() != 0 {
			t.Fatalf("polled too early: %d fetches, %v", e.fetches(), err)
		}

		// Due, and the provider has not seen the transaction ("Not Found" for a
		// txnid not submitted yet): the attempt stays open and polling goes on.
		e.clk.Add(3 * time.Minute)
		if err := e.flow.PollDue(ctx); err != nil {
			t.Fatal(err)
		}
		if e.fetches() != 1 {
			t.Fatalf("fetches = %d, want 1", e.fetches())
		}
		a := e.attempts(p.ID)[0]
		if a.Status != "created" || e.get(p.ID).Status == "failed" {
			t.Fatalf("a payment the provider has not seen yet was settled early: attempt %s, payment %s", a.Status, e.get(p.ID).Status)
		}
		next2, count2 := e.pollInfo(st.Attempt.ID)
		if count2 != 1 || !next2.After(e.clk.Now()) {
			t.Fatalf("polling stopped after a not-found: next=%v count=%d", next2, count2)
		}

		// The customer paid, and neither the return nor a webhook ever arrives.
		e.setPayment(st.Attempt.ProviderRef, succeeded(50000, "INR", "pp_lost"))
		e.clk.Add(10 * time.Minute)
		if err := e.flow.PollDue(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status != "succeeded" || got.AmountReceived != 50000 {
			t.Fatalf("payment = %+v, want succeeded by polling alone", got)
		}
		if a := e.attempts(p.ID)[0]; a.Status != "captured" || a.ProviderPaymentID != "pp_lost" {
			t.Errorf("attempt = %+v", a)
		}
		// Terminal: no more polling.
		before := e.fetches()
		e.clk.Add(2 * time.Hour)
		_ = e.flow.PollDue(ctx)
		if e.fetches() != before {
			t.Error("a captured attempt is still polled")
		}
	})
}

// Polling is bounded: it runs through the checkout's expiry plus a grace
// period and then stops, whatever the provider says.
func TestCheckoutPollingIsBounded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.formPostCheckout()
		p := e.payment2()
		if _, err := e.flow.StartCheckout(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		start := e.clk.Now()
		fetches := func() int { return e.fetches() }
		var last int
		for min := 1; min <= 6*60; min++ {
			e.clk.Add(time.Minute)
			if err := e.flow.PollDue(ctx); err != nil {
				t.Fatal(err)
			}
			if min == int(checkout.CheckoutPollGrace/time.Minute)+int((15*time.Minute)/time.Minute)+1 {
				last = fetches()
			}
		}
		if last == 0 {
			t.Fatal("never polled")
		}
		if fetches() != last {
			t.Errorf("polling went on after the checkout's expiry and the grace period: %d fetches at the end, %d at the bound (started %v)", fetches(), last, start)
		}
		if last > 12 {
			t.Errorf("%d polls in the window: the schedule does not back off", last)
		}
	})
}

// Expiring a payment fetches from the provider first: success wins, an error
// means "not now", and only "nothing there" lets the payment expire.
func TestExpiryAsksTheProviderFirst(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.core.SetAttemptSyncer(e.flow)
		e.formPostCheckout()
		paid := e.payment2()
		stPaid, err := e.flow.StartCheckout(ctx, paid.ID)
		if err != nil {
			t.Fatal(err)
		}
		unreachable := e.payment2()
		if _, err := e.flow.StartCheckout(ctx, unreachable.ID); err != nil {
			t.Fatal(err)
		}
		unpaid := e.payment2()
		if _, err := e.flow.StartCheckout(ctx, unpaid.ID); err != nil {
			t.Fatal(err)
		}
		// The payment's time is over and the poller has not run: the provider has the money.
		e.clk.Add(2 * time.Hour)
		e.setPayment(stPaid.Attempt.ProviderRef, succeeded(50000, "INR", "pp_late"))
		// For `unreachable` the provider errors; for the others it answers.
		e.fake.Locked(func() { e.fake.FetchErr = errors.New("provider is down") })
		if n, err := e.core.ExpireDue(ctx, 100); err != nil || n != 0 {
			t.Fatalf("with the provider down nothing may be expired or settled: n=%d err=%v", n, err)
		}
		for _, id := range []string{paid.ID, unreachable.ID, unpaid.ID} {
			if s := e.get(id).Status; s == "expired" || s == "failed" {
				t.Fatalf("payment %s became %s while the provider could not be asked", id, s)
			}
		}
		e.fake.Locked(func() { e.fake.FetchErr = nil })
		n, err := e.core.ExpireDue(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.get(paid.ID); got.Status != "succeeded" || got.AmountReceived != 50000 {
			t.Fatalf("the payment the provider says is paid was %s, want succeeded", got.Status)
		}
		if e.get(unpaid.ID).Status != "expired" || e.get(unreachable.ID).Status != "expired" || n != 2 {
			t.Errorf("payments the provider has not seen should expire: unpaid=%s unreachable=%s n=%d",
				e.get(unpaid.ID).Status, e.get(unreachable.ID).Status, n)
		}
	})
}

// A success that reaches the provider after the payment expired (the provider
// had not seen the transaction yet at expiry) is still applied by polling,
// inside the grace period.
func TestSuccessAfterExpiryIsAppliedByPolling(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.core.SetAttemptSyncer(e.flow)
		e.formPostCheckout()
		p := e.payment2()
		st, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		// At the payment's expiry the provider has nothing: it expires.
		e.clk.Add(61 * time.Minute)
		if n, err := e.core.ExpireDue(ctx, 100); err != nil || n != 1 {
			t.Fatalf("expire: n=%d err=%v", n, err)
		}
		if e.get(p.ID).Status != "expired" {
			t.Fatalf("status %s", e.get(p.ID).Status)
		}
		// The customer's slow 3DS completes later; still inside the grace period.
		e.setPayment(st.Attempt.ProviderRef, succeeded(50000, "INR", "pp_slow"))
		e.clk.Add(5 * time.Minute)
		if err := e.flow.PollDue(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status != "succeeded" || got.AmountReceived != 50000 {
			t.Fatalf("a late success was lost: %+v", got)
		}
	})
}
