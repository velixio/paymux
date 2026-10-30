package checkout_test

import (
	"context"
	"errors"
	"testing"

	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/payouts"
	"github.com/velixio/paymux/pkg/connector"
)

type stubPayouts struct {
	err   error
	calls []string
}

func (p *stubPayouts) SyncNotification(_ context.Context, account string, n connector.Notification) error {
	p.calls = append(p.calls, account+"/"+n.Payout.ProviderRef)
	return p.err
}

func TestPayoutNotificationsGoToThePayoutsService(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		stub := &stubPayouts{}
		flow := checkout.New(checkout.Options{DB: e.db, Core: e.core, Conns: e.conns, Routing: checkout.NewRouting(e.breaker), Breaker: e.breaker,
			Now: e.clk.Now, Payouts: stub})
		_, st := startedAttempt(t, e)
		note := []connector.Notification{{Kind: connector.NotifyPayout, EventID: "po1", EventType: "payout.processed",
			Payout: &connector.PayoutSnapshot{ProviderRef: "po_r1"}}}
		ev := storeEvent(t, e, st.AccountID, true, note)
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if len(stub.calls) != 1 || stub.calls[0] != st.AccountID+"/po_r1" {
			t.Errorf("calls %v", stub.calls)
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects") != 0 {
			t.Error("a handled payout was recorded as unknown")
		}
		// A payout that is not known here is recorded for an operator.
		stub.err = payouts.ErrUnknownPayout
		note[0].EventID, note[0].Payout.ProviderRef = "po2", "po_nobody"
		ev = storeEvent(t, e, st.AccountID, true, note)
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects WHERE kind = 'payout'") != 1 {
			t.Error("an unknown payout was not recorded")
		}
		// Anything else is retried.
		stub.err = errors.New("db down")
		note[0].EventID = "po3"
		ev = storeEvent(t, e, st.AccountID, true, note)
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err == nil {
			t.Error("a transient failure must make the job retry")
		}
		// Without a payouts service the notification is only recorded.
		bare := checkout.New(checkout.Options{DB: e.db, Core: e.core, Conns: e.conns, Routing: checkout.NewRouting(e.breaker), Breaker: e.breaker, Now: e.clk.Now})
		note[0].EventID = "po4"
		ev = storeEvent(t, e, st.AccountID, true, note)
		if err := bare.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects WHERE kind = 'payout'") != 2 {
			t.Error("without a payouts service the payout must be recorded as unknown")
		}
	})
}
