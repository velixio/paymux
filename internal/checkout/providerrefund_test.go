package checkout_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

func (e *env) providerRefund(key string, snap connector.RefundSnapshot) {
	e.fake.Locked(func() { e.fake.Refunds[key] = snap })
}

func refundHook(t *testing.T, e *env, account, eventID, refundID string, claimed int64) string {
	t.Helper()
	// The webhook's own amount is a lie on purpose: the fetched truth wins.
	return storeEvent(t, e, account, true, []connector.Notification{{Kind: connector.NotifyRefund, EventID: eventID, EventType: "refund.processed",
		Refund: &connector.RefundSnapshot{ProviderRefundID: refundID, Status: connector.RefundSucceeded, Amount: claimed, Currency: "INR"}}})
}

func refundRows(e *env, paymentID string) []store.Refund {
	e.t.Helper()
	var out []store.Refund
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.ListRefunds(ctx, q, paymentID); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestARefundMadeAtTheProviderIsBookedFromTheFetchedTruth(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := paid(t, e)
		e.providerRefund("dash_1", connector.RefundSnapshot{ProviderRefundID: "rfnd_dash", ProviderPaymentID: "pay_1",
			Status: connector.RefundSucceeded, RawStatus: "processed", Amount: 12000, Currency: "INR"})

		ev := refundHook(t, e, st.AccountID, "r1", "rfnd_dash", 999)
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		rows := refundRows(e, p.ID)
		if len(rows) != 1 {
			t.Fatalf("refunds: %+v", rows)
		}
		r := rows[0]
		if r.InitiatedBy != "provider" || r.ProviderRefundID != "rfnd_dash" || r.Amount != 12000 || r.Status != "succeeded" || r.AttemptID != st.Attempt.ID {
			t.Errorf("refund %+v (the amount must be the fetched 12000, not the webhook's 999)", r)
		}
		if got := e.get(p.ID); got.AmountRefunded != 12000 || !got.HasFlag("refunded_partial") {
			t.Errorf("payment %+v", got)
		}
		evs := e.emitted()
		if !slices.Contains(evs, "refund.created") || evs[len(evs)-1] != "refund.succeeded" {
			t.Errorf("events %v", evs)
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects") != 0 {
			t.Error("a refund of ours was recorded as unknown")
		}
		e.fake.Locked(func() {
			if len(e.fake.RefundCalls) != 0 {
				t.Errorf("Paymux asked the provider to refund again: %+v", e.fake.RefundCalls)
			}
		})

		// The provider delivers it again (a retry, a second event): nothing changes.
		before := len(e.emitted())
		ev = refundHook(t, e, st.AccountID, "r2", "rfnd_dash", 12000)
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if len(refundRows(e, p.ID)) != 1 || e.get(p.ID).AmountRefunded != 12000 || len(e.emitted()) != before {
			t.Errorf("a second delivery changed state: %+v", refundRows(e, p.ID))
		}
	})
}

func TestAProviderRefundOfAnUnknownPaymentStaysUnknown(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := paid(t, e)
		e.providerRefund("stranger", connector.RefundSnapshot{ProviderRefundID: "rfnd_stranger", ProviderPaymentID: "pay_not_ours",
			Status: connector.RefundSucceeded, Amount: 500, Currency: "INR"})
		e.providerRefund("nopay", connector.RefundSnapshot{ProviderRefundID: "rfnd_nopay", Status: connector.RefundSucceeded, Amount: 500, Currency: "INR"})
		e.providerRefund("usd", connector.RefundSnapshot{ProviderRefundID: "rfnd_usd", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 500, Currency: "USD"})
		for i, id := range []string{"rfnd_stranger", "rfnd_nopay", "rfnd_usd"} {
			ev := refundHook(t, e, st.AccountID, "u"+string(rune('a'+i)), id, 500)
			if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
		if len(refundRows(e, p.ID)) != 0 || e.get(p.ID).AmountRefunded != 0 {
			t.Errorf("an unbookable refund changed our books: %+v", refundRows(e, p.ID))
		}
		var objs []store.UnknownObject
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { objs, err = store.ListUnknownObjects(ctx, q, st.AccountID); return })
		ids := map[string]bool{}
		for _, o := range objs {
			ids[o.ProviderID] = o.Kind == "refund"
		}
		if !ids["rfnd_stranger"] || !ids["rfnd_nopay"] || !ids["rfnd_usd"] {
			t.Errorf("unknown objects %+v", objs)
		}
	})
}

// A payment of another account is not ours either, even with a matching id.
func TestAProviderRefundIsMatchedOnlyToTheDeliveringAccount(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := paid(t, e)
		other := e.otherAccount(st.AccountID)
		e.providerRefund("x", connector.RefundSnapshot{ProviderRefundID: "rfnd_x", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 500, Currency: "INR"})
		ev := refundHook(t, e, other, "o1", "rfnd_x", 500)
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(other, ev)); err != nil {
			t.Fatal(err)
		}
		if len(refundRows(e, p.ID)) != 0 {
			t.Error("another account's refund notification reached our payment")
		}
	})
}

func TestAnOverRefundAtTheProviderIsRecordedAndFlagged(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := paid(t, e) // 50000 received
		e.providerRefund("big", connector.RefundSnapshot{ProviderRefundID: "rfnd_big", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 60000, Currency: "INR"})
		ev := refundHook(t, e, st.AccountID, "big1", "rfnd_big", 60000)
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		got := e.get(p.ID)
		if got.AmountRefunded != 60000 || !got.HasFlag("refund_exceeds_balance") {
			t.Errorf("payment %+v", got)
		}
		if evs := e.emitted(); !slices.Contains(evs, "payment.needs_review") {
			t.Errorf("events %v", evs)
		}
	})
}

func TestAProviderRefundIsNeverSubmittedToTheProvider(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := paid(t, e)
		e.providerRefund("slow", connector.RefundSnapshot{ProviderRefundID: "rfnd_slow", ProviderPaymentID: "pay_1", Status: connector.RefundPending, Amount: 700, Currency: "INR"})
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyRefund, EventID: "s1", EventType: "refund.created",
			Refund: &connector.RefundSnapshot{ProviderRefundID: "rfnd_slow", Status: connector.RefundPending}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		rows := refundRows(e, p.ID)
		if len(rows) != 1 || rows[0].Status != "pending" || rows[0].InitiatedBy != "provider" {
			t.Fatalf("refunds %+v", rows)
		}
		// Even if a refund.submit job exists for it (it must not), running one sends nothing.
		payload, _ := json.Marshal(map[string]string{"refund_id": rows[0].ID})
		if err := e.flow.SubmitRefund(ctx, payload); err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			if len(e.fake.RefundCalls) != 0 {
				t.Errorf("a refund the provider made was sent to it again: %+v", e.fake.RefundCalls)
			}
		})
		if e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit'") != 0 {
			t.Error("a refund.submit job was queued for a provider refund")
		}
		// It completes when the provider says so.
		e.providerRefund("slow", connector.RefundSnapshot{ProviderRefundID: "rfnd_slow", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 700, Currency: "INR"})
		ev = refundHook(t, e, st.AccountID, "s2", "rfnd_slow", 700)
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.AmountRefunded != 700 || len(refundRows(e, p.ID)) != 1 {
			t.Errorf("completion: %+v rows %d", got, len(refundRows(e, p.ID)))
		}
	})
}
