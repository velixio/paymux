package checkout_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

type sink struct {
	mu         sync.Mutex
	notes      []connector.Notification
	accounts   []string
	reconciled []string
	err        error
	returnURL  string
	handled    bool
}

func (s *sink) HandleNotification(_ context.Context, account string, n connector.Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes, s.accounts = append(s.notes, n), append(s.accounts, account)
	return s.err
}

func (s *sink) ReconcilePayment(_ context.Context, paymentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciled = append(s.reconciled, paymentID)
	return nil
}

func (s *sink) ReturnRedirect(context.Context, string, string) (string, bool, error) {
	return s.returnURL, s.handled, nil
}

func (e *env) flowWith(s checkout.SubscriptionSink) *checkout.Service {
	return checkout.New(checkout.Options{DB: e.db, Core: e.core, Conns: e.conns, Routing: checkout.NewRouting(e.breaker),
		Breaker: e.breaker, Now: e.clk.Now, Subs: s})
}

func TestSubscriptionAndMandateNotificationsGoToTheSubscriptionsEngine(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, st := startedAttempt(t, e)
		sk := &sink{}
		flow := e.flowWith(sk)
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{
			{Kind: connector.NotifySubscription, EventID: "s1", Subscription: &connector.SubscriptionSnapshot{ProviderSubscriptionID: "sub_p1", Status: "active"}},
			{Kind: connector.NotifyMandate, EventID: "m1", Mandate: &connector.MandateSnapshot{ProviderMandateID: "mdt_p1", Status: "active"}},
		})
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if len(sk.notes) != 2 || sk.accounts[0] != st.AccountID || sk.notes[0].Subscription.ProviderSubscriptionID != "sub_p1" || sk.notes[1].Mandate.ProviderMandateID != "mdt_p1" {
			t.Fatalf("notes = %+v accounts = %v", sk.notes, sk.accounts)
		}
		// Without a sink they are only recorded as unknown (the old behaviour).
		ev = storeEvent(t, e, st.AccountID, true, []connector.Notification{
			{Kind: connector.NotifySubscription, EventID: "s2", Subscription: &connector.SubscriptionSnapshot{ProviderSubscriptionID: "sub_p2"}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		var objs []store.UnknownObject
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { objs, err = store.ListUnknownObjects(ctx, q, st.AccountID); return })
		if len(objs) != 1 || objs[0].Kind != "subscription" {
			t.Errorf("unknown objects = %+v", objs)
		}
	})
}

func TestUnknownSubscriptionObjectsAreRecordedAndOtherErrorsRetried(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, st := startedAttempt(t, e)
		sk := &sink{err: fmt.Errorf("%w: not ours", checkout.ErrUnknownObject)}
		flow := e.flowWith(sk)
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{
			{Kind: connector.NotifySubscription, EventID: "s3", Subscription: &connector.SubscriptionSnapshot{ProviderSubscriptionID: "sub_stranger"}}})
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		var objs []store.UnknownObject
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { objs, err = store.ListUnknownObjects(ctx, q, st.AccountID); return })
		if len(objs) != 1 || objs[0].ProviderID != "sub_stranger" {
			t.Errorf("unknown objects = %+v", objs)
		}
		// Any other failure bubbles up so the job queue retries it.
		sk.err = errors.New("db down")
		ev = storeEvent(t, e, st.AccountID, true, []connector.Notification{
			{Kind: connector.NotifyMandate, EventID: "m2", Mandate: &connector.MandateSnapshot{ProviderMandateID: "mdt_x"}}})
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err == nil {
			t.Error("transient failure swallowed")
		}
	})
}

func TestPaymentNotificationReconcilesRenewals(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		e.setPayment(st.Attempt.ProviderRef, succeeded(50000, "INR", "pay_9"))
		sk := &sink{}
		flow := e.flowWith(sk)
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "p1",
			Payment: &connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, Status: connector.PaymentSucceeded}}})
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if len(sk.reconciled) != 1 || sk.reconciled[0] != p.ID {
			t.Errorf("reconciled = %v", sk.reconciled)
		}
	})
}

func TestSubscriptionReturn(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if url, handled, err := e.flow.SubscriptionReturn(ctx, "pa", "ref"); url != "" || handled || err != nil {
			t.Errorf("without a sink: %q %v %v", url, handled, err)
		}
		flow := e.flowWith(&sink{returnURL: "https://app.example.com/done?x=1", handled: true})
		if url, handled, err := flow.SubscriptionReturn(ctx, "pa", "ref"); url != "https://app.example.com/done?x=1" || !handled || err != nil {
			t.Errorf("with a sink: %q %v %v", url, handled, err)
		}
	})
}
