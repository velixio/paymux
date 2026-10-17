package checkout_test

import (
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/pkg/connector"
)

// What a provider returns for an id is only ever applied to the attempt it
// belongs to. A payment id that arrives with the customer's browser, or in a
// notification, is a pointer to look at, not a fact.

func TestAnotherCustomersPaymentIsNeverAppliedToOurAttempt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		// A genuine, paid, same-amount payment that belongs to somebody else's checkout.
		e.setPayment("somebody_elses_ref", connector.PaymentSnapshot{ProviderRef: "somebody_elses_ref", ProviderOrderID: "order_foreign",
			ProviderPaymentID: "pay_foreign", Status: connector.PaymentSucceeded, RawStatus: "captured", Amount: 50000, Currency: "INR"})

		// The return URL is a plain browser request; its payment_id can say anything.
		got, err := e.flow.HandleReturn(ctx, st.AccountID, st.Attempt.ProviderRef, checkout.ReturnInput{Method: "GET",
			Form: map[string][]string{"sig": {"ok"}, "payment_id": {"pay_foreign"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "succeeded" || e.get(p.ID).Status == "succeeded" {
			t.Fatal("a foreign payment marked our payment paid")
		}
		a, _ := e.core.GetAttempt(ctx, st.Attempt.ID)
		if a.ProviderPaymentID != "" || a.Status == "captured" {
			t.Errorf("our attempt took a foreign payment id: %+v", a)
		}

		// The same through a notification that names our attempt but a foreign payment.
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "n1",
			Payment: &connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, ProviderPaymentID: "pay_foreign", Status: connector.PaymentSucceeded}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if e.get(p.ID).Status == "succeeded" {
			t.Fatal("a notification pointed our attempt at a foreign payment")
		}
	})
}

func TestAPaymentOfAnotherOrderIsNeverAppliedEvenUnderOurReference(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		if st.Attempt.ProviderOrderID == "" {
			e.fake.Locked(func() {})
			t.Skip("the fake connector gave no order id")
		}
		e.setPayment(st.Attempt.ProviderRef, connector.PaymentSnapshot{ProviderOrderID: "order_of_another_checkout", ProviderPaymentID: "pay_x",
			Status: connector.PaymentSucceeded, Amount: 50000, Currency: "INR"})
		if _, err := e.flow.SyncAttempt(ctx, st.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		if e.get(p.ID).Status == "succeeded" {
			t.Fatal("a payment of another order marked ours paid")
		}
	})
}

func TestARefundThatIsNotOursIsNeverApplied(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paid(t, e)
		mine, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 20000})
		if err != nil {
			t.Fatal(err)
		}
		cases := map[string]connector.RefundSnapshot{
			"another payment": {ProviderRefundID: "rfnd_a", ProviderPaymentID: "pay_other", Status: connector.RefundSucceeded, Amount: 20000, Currency: "INR"},
			"another amount":  {ProviderRefundID: "rfnd_b", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 5, Currency: "INR"},
			"another receipt": {ProviderRef: "someone-elses-receipt", ProviderRefundID: "rfnd_c", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 20000, Currency: "INR"},
		}
		for name, snap := range cases {
			// The provider answers our lookup with a refund that is not this one.
			e.fake.Locked(func() { e.fake.Refunds = map[string]connector.RefundSnapshot{mine.ProviderRef: snap} })
			_, _ = e.flow.SyncRefund(ctx, mine.ID)
			got, _ := e.core.GetRefund(ctx, e.scope, mine.ID)
			if got.Status != "pending" || e.get(p.ID).AmountRefunded != 0 {
				t.Errorf("%s: our refund was moved by a refund that is not ours: %+v", name, got)
			}
		}
		// The real one still applies.
		e.fake.Locked(func() {
			e.fake.Refunds = map[string]connector.RefundSnapshot{mine.ProviderRef: {ProviderRefundID: "rfnd_real", ProviderPaymentID: "pay_1",
				Status: connector.RefundSucceeded, Amount: 20000, Currency: "INR"}}
		})
		if _, err := e.flow.SyncRefund(ctx, mine.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.core.GetRefund(ctx, e.scope, mine.ID); got.Status != "succeeded" || e.get(p.ID).AmountRefunded != 20000 {
			t.Errorf("the real refund: %+v", got)
		}
	})
}
