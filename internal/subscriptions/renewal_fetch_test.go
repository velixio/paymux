package subscriptions_test

import (
	"errors"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

// activated returns a provider-managed subscription that is active, and the
// renewal notification the provider sent for it (subscription.charged).
func activatedWithCharge(t *testing.T, e *env) (store.Subscription, connector.Notification) {
	t.Helper()
	sub := managed(t, e)
	for _, n := range rzp(t, "event_subscription_activated") {
		if err := e.notify("pa_managed", n); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range rzp(t, "event_subscription_charged") {
		if n.Payment != nil {
			return sub, n
		}
	}
	t.Fatal("no payment in the charged fixture")
	return sub, connector.Notification{}
}

// truth is what the provider's API says of the payment the notification names.
func truth(n connector.Notification) connector.PaymentSnapshot { return *n.Payment }

func TestRenewalBooksTheFetchedPaymentNotTheWebhookBody(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub, n := activatedWithCharge(t, e)
		real := truth(n)
		e.r.mu.Lock()
		e.r.payTruth = map[string]connector.PaymentSnapshot{real.ProviderPaymentID: real}
		e.r.mu.Unlock()
		forged := *n.Payment
		forged.Amount, forged.Currency = 100, "USD" // what the body claims
		n.Payment = &forged
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
			t.Fatal(err)
		}
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Amount != real.Amount || pays[0].Currency != real.Currency || pays[0].AmountReceived != real.Amount {
			t.Fatalf("payments = %+v, want %d %s", pays, real.Amount, real.Currency)
		}
	})
}

func TestRenewalOfAnotherSubscriptionsPaymentBooksNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub, n := activatedWithCharge(t, e)
		other := truth(n)
		other.SubscriptionID = "sub_someone_else"
		e.r.mu.Lock()
		e.r.payTruth = map[string]connector.PaymentSnapshot{other.ProviderPaymentID: other}
		e.r.mu.Unlock()
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
			t.Fatalf("a payment of another subscription is dropped, not retried: %v", err)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 0 {
			t.Fatalf("booked %+v", pays)
		}
		if got := e.sub(sub.ID); got.RenewalsDone != 0 {
			t.Errorf("renewals = %d", got.RenewalsDone)
		}
	})
}

func TestRenewalFetchErrorIsRetriedAndBooksNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub, n := activatedWithCharge(t, e)
		e.r.mu.Lock()
		e.r.payFn = func(connector.PaymentRef) (connector.PaymentSnapshot, error) {
			return connector.PaymentSnapshot{}, connector.ErrTemporary
		}
		e.r.mu.Unlock()
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); !errors.Is(err, connector.ErrTemporary) {
			t.Fatalf("fetch error: %v", err)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 0 {
			t.Fatalf("booked %+v", pays)
		}
		// The retry, with the provider back, books it once.
		real := truth(n)
		e.r.mu.Lock()
		e.r.payFn = func(connector.PaymentRef) (connector.PaymentSnapshot, error) { return real, nil }
		e.r.mu.Unlock()
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
			t.Fatal(err)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 {
			t.Fatalf("payments = %+v", pays)
		}
	})
}

func TestRenewalTheProviderDoesNotKnowBooksNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub, n := activatedWithCharge(t, e) // payTruth is empty: not found
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
			t.Fatalf("not found is final: %v", err)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 0 {
			t.Fatalf("booked %+v", pays)
		}
	})
}

// A payment the provider has not captured (yet) is not booked as captured; the
// job is retried so a late capture is still seen.
func TestRenewalTheProviderSaysIsNotCapturedIsRetried(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub, n := activatedWithCharge(t, e)
		pending := truth(n)
		pending.Status = connector.PaymentProcessing
		e.r.mu.Lock()
		e.r.payTruth = map[string]connector.PaymentSnapshot{pending.ProviderPaymentID: pending}
		e.r.mu.Unlock()
		if err := e.subs.HandleNotification(ctx, "pa_managed", n); err == nil {
			t.Fatal("accepted a payment the provider has not captured")
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 0 {
			t.Fatalf("booked %+v", pays)
		}
	})
}
