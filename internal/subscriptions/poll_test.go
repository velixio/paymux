package subscriptions_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

func (e *env) fetches() int { e.r.mu.Lock(); defer e.r.mu.Unlock(); return len(e.r.fetchCalls) }

func (e *env) setProviderMandate(id string, snap connector.MandateSnapshot) {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	e.r.mandates[id] = snap
}

// A customer who never comes back from the provider's page is found by the
// poller, on the 1m, 2m, 5m, 15m ladder.
func TestPendingMandateIsPolledOnTheLadderUntilItSettles(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.sub(c.Subscription.ID)
		m := e.mandate(sub.MandateID)
		e.setProviderMandate(m.ProviderRef, connector.MandateSnapshot{ProviderMandateID: "pm_poll", Status: "pending", RawStatus: "pending"})

		at := func(minutes int) { e.advanceTo(t0.Add(time.Duration(minutes) * time.Minute)) }
		at(0)
		if e.fetches() != 0 {
			t.Fatal("polled before the first delay")
		}
		at(1)
		if e.fetches() != 1 {
			t.Fatalf("polls at +1m = %d", e.fetches())
		}
		at(2)
		if e.fetches() != 1 {
			t.Fatal("polled again after only 1 more minute (the ladder is 1m, 2m, 5m, ...)")
		}
		at(3)
		at(7)
		if e.fetches() != 2 {
			t.Fatalf("polls by +7m = %d, want 2 (at +1m and +3m)", e.fetches())
		}
		at(8)
		if e.fetches() != 3 {
			t.Fatalf("polls at +8m = %d", e.fetches())
		}
		if got := e.sub(sub.ID); got.Status != "incomplete" {
			t.Fatalf("status = %s while the provider says pending", got.Status)
		}

		// The customer paid but never returned: the next poll finds it.
		e.setProviderMandate(m.ProviderRef, connector.MandateSnapshot{ProviderMandateID: "pm_poll", Status: "active", RawStatus: "confirmed", Currency: "INR",
			Method: "upi", SetupPayment: &connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 49900, Currency: "INR",
				ProviderPaymentID: "setup_poll", ProviderRef: m.ProviderRef}})
		at(23)
		got := e.sub(sub.ID)
		if got.Status != "active" || got.RenewalsDone != 1 {
			t.Fatalf("after the poll: %+v", got)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 || pays[0].Status != "succeeded" {
			t.Errorf("payments = %+v", pays)
		}
		if got := e.mandate(m.ID); got.Status != "active" || got.ProviderMandateID != "pm_poll" {
			t.Errorf("mandate = %+v", got)
		}
		// Settled: no more polls.
		n := e.fetches()
		e.drive(3*time.Hour, 10*time.Minute)
		if e.fetches() != n {
			t.Errorf("kept polling a settled mandate: %d -> %d", n, e.fetches())
		}
	})
}

// A customer who never completes: the polls stop with the 24h window and the
// subscription expires as before.
func TestMandatePollingStopsWithTheIncompleteExpiry(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		e.drive(26*time.Hour, 10*time.Minute)
		// 1m, 3m, 8m, 23m, 83m, 443m: the next (1883m) is beyond 24h.
		if e.fetches() != 6 {
			t.Fatalf("polls = %d, want 6", e.fetches())
		}
		sub := e.sub(c.Subscription.ID)
		if sub.Status != "incomplete_expired" {
			t.Errorf("status = %s", sub.Status)
		}
		if m := e.mandate(sub.MandateID); m.Status != "expired" {
			t.Errorf("mandate = %s", m.Status)
		}
	})
}

func TestPollFindsAFailedRegistration(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.sub(c.Subscription.ID)
		m := e.mandate(sub.MandateID)
		e.setProviderMandate(m.ProviderRef, connector.MandateSnapshot{ProviderMandateID: "pm_bad", Status: "failed", RawStatus: "failure"})
		e.advanceTo(t0.Add(time.Minute))
		if got := e.mandate(m.ID); got.Status != "failed" {
			t.Fatalf("mandate = %s", got.Status)
		}
		if got := e.sub(sub.ID); got.Status != "incomplete" {
			t.Errorf("status = %s", got.Status)
		}
	})
}

// Replacement mandates (registration retry, re-authorization, new payment
// method) are polled too.
func TestReplacementMandatesArePolled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := hostedServer(e)
		c := e.mustCreate(nil)
		u, _ := url.Parse(c.CheckoutURL)
		first := e.mandate(e.sub(c.Subscription.ID).MandateID)
		e.failMandate(first, "pm_failed")
		fetch(t, srv.URL+u.RequestURI()) // retry: a fresh mandate
		second := e.mandate(e.sub(c.Subscription.ID).MandateID)
		if second.ID == first.ID {
			t.Fatal("no fresh mandate")
		}
		e.setProviderMandate(second.ProviderRef, connector.MandateSnapshot{ProviderMandateID: "pm_second", Status: "active", RawStatus: "ok", Currency: "INR",
			SetupPayment: &connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 49900, Currency: "INR", ProviderPaymentID: "setup_2"}})
		e.advanceTo(t0.Add(2 * time.Minute))
		if got := e.sub(c.Subscription.ID); got.Status != "active" || got.MandateID != second.ID {
			t.Fatalf("sub = %+v", got)
		}
	})
}
