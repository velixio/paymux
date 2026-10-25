package checkout_test

import (
	"testing"

	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/pkg/connector"
)

// AUDIT (F3 spot-check): the core's own ownership check (snapshotIsOurs) only
// rejects a snapshot that contradicts the attempt. A connector that returns a
// payment which names neither a reference nor an order (Zoho for a payment with
// no session/reference, any connector that omits the fields) is applied to the
// attempt on the strength of an unsigned return-URL payment_id alone.
func TestAuditPayCoreAppliesForeignSnapshotThatNamesNoReferenceOrOrder(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		// Key "" so the fake does not stamp a reference on it: a paid,
		// same-amount payment of the same provider account that is not ours and
		// names no reference and no order.
		e.setPayment("", connector.PaymentSnapshot{ProviderPaymentID: "pay_foreign", Status: connector.PaymentSucceeded,
			RawStatus: "captured", Amount: 50000, Currency: "INR"})
		_, err := e.flow.HandleReturn(ctx, st.AccountID, st.Attempt.ProviderRef, checkout.ReturnInput{Method: "GET",
			Form: map[string][]string{"sig": {"ok"}, "payment_id": {"pay_foreign"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status == "succeeded" {
			t.Fatalf("a snapshot naming no reference or order (fetched by a return-URL payment id) marked our payment paid: %+v", got)
		}
	})
}

// AUDIT: nothing stops one provider payment from settling two Paymux payments.
// provider_payment_id is not unique per account (a plain index), and
// ApplyAttemptSnapshot never asks whether another attempt already holds it.
// With a connector whose ownership check is fail-open (Zoho, above), one real
// payment confirms any number of same-amount Paymux payments.
func TestAuditPayOneProviderPaymentSettlesTwoPayments(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p1, st1 := startedAttempt(t, e)
		p2, st2 := startedAttempt(t, e)
		e.setPayment("", connector.PaymentSnapshot{ProviderPaymentID: "pay_shared", Status: connector.PaymentSucceeded,
			RawStatus: "captured", Amount: 50000, Currency: "INR"})
		for _, st := range []checkout.Started{st1, st2} {
			if _, err := e.flow.HandleReturn(ctx, st.AccountID, st.Attempt.ProviderRef, checkout.ReturnInput{Method: "GET",
				Form: map[string][]string{"sig": {"ok"}, "payment_id": {"pay_shared"}}}); err != nil {
				t.Fatal(err)
			}
		}
		if e.get(p1.ID).Status == "succeeded" && e.get(p2.ID).Status == "succeeded" {
			t.Fatalf("provider payment pay_shared settled both %s and %s", p1.ID, p2.ID)
		}
	})
}
