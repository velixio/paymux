package checkout_test

import (
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

func triesOf(t *testing.T, e *env, paymentID string) []store.ProviderPaymentAttempt {
	t.Helper()
	var out []store.ProviderPaymentAttempt
	if err := e.db.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.ListProviderPaymentAttempts(ctx, q, paymentID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWebhookRecordsNotifiedFailedTry(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		// The fetch shows only the customer's latest go; the event names the earlier one.
		e.setPayment(ref, connector.PaymentSnapshot{ProviderRef: ref, ProviderPaymentID: "pi_1", TryID: "ch_2", Status: connector.PaymentRequiresAction,
			RawStatus: "requires_payment_method", Amount: 50000, Currency: "INR", ErrorCode: "card_declined", ErrorMessage: "Second."})
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "e1", EventType: "payment_intent.payment_failed",
			Payment: &connector.PaymentSnapshot{ProviderRef: ref, ProviderPaymentID: "pi_1", TryID: "ch_1", Status: connector.PaymentRequiresAction,
				RawStatus: "requires_payment_method", Amount: 50000, Currency: "INR", ErrorCode: "insufficient_funds", ErrorMessage: "First."}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		byTry := map[string]store.ProviderPaymentAttempt{}
		for _, try := range triesOf(t, e, p.ID) {
			byTry[try.TryID] = try
		}
		if byTry["ch_1"].FailureMessage != "First." || byTry["ch_2"].FailureMessage != "Second." {
			t.Fatalf("tries = %+v", byTry)
		}
	})
}

func TestUnsignedWebhookDoesNotRecordItsOwnClaim(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		e.setPayment(ref, connector.PaymentSnapshot{ProviderRef: ref, Status: connector.PaymentRequiresAction, RawStatus: "created", Amount: 50000, Currency: "INR"})
		ev := storeEvent(t, e, st.AccountID, false, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "u1",
			Payment: &connector.PaymentSnapshot{ProviderRef: ref, ProviderPaymentID: "pi_forged", TryID: "ch_forged", Status: connector.PaymentFailed, ErrorCode: "forged"}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if got := triesOf(t, e, p.ID); len(got) != 0 {
			t.Fatalf("an unsigned claim became history: %+v", got)
		}
	})
}

func TestWebhookFetchesTryHistoryOnFailure(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		order := st.Attempt.ProviderOrderID
		if order == "" {
			t.Fatal("the fake checkout should give the attempt an order")
		}
		e.setPayment(ref, connector.PaymentSnapshot{ProviderRef: ref, ProviderOrderID: order, ProviderPaymentID: "pay_2", Status: connector.PaymentRequiresAction,
			RawStatus: "created", Amount: 50000, Currency: "INR"})
		e.setPayment("older", connector.PaymentSnapshot{ProviderRef: ref, ProviderOrderID: order, ProviderPaymentID: "pay_1", Status: connector.PaymentFailed,
			RawStatus: "failed", Amount: 50000, Currency: "INR", ErrorCode: "declined", ErrorMessage: "Declined."})
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "e1", EventType: "payment.failed",
			Payment: &connector.PaymentSnapshot{ProviderRef: ref, ProviderOrderID: order, Status: connector.PaymentFailed, ErrorCode: "declined"}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, try := range triesOf(t, e, p.ID) {
			found = found || (try.ProviderPaymentID == "pay_1" && try.FailureMessage == "Declined.")
		}
		if !found {
			t.Fatalf("tries = %+v", triesOf(t, e, p.ID))
		}
	})
}

func TestStoredAttemptDoesNotDuplicateATry(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		// A provider retrying within one payment id: the attempt stores the id, the try its own key.
		e.setPayment(ref, connector.PaymentSnapshot{ProviderRef: ref, ProviderPaymentID: "pi_1", TryID: "ch_1", Status: connector.PaymentRequiresAction,
			RawStatus: "requires_payment_method", Amount: 50000, Currency: "INR", ErrorCode: "card_declined"})
		for range 2 {
			if _, err := e.flow.SyncAttemptLatest(ctx, st.Attempt.ID); err != nil {
				t.Fatal(err)
			}
		}
		got := triesOf(t, e, p.ID)
		if len(got) != 1 || got[0].TryID != "ch_1" {
			t.Fatalf("tries = %+v, want only ch_1", got)
		}
	})
}
