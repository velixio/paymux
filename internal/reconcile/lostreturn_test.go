package reconcile_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/core"
)

// A checkout whose customer paid at the provider but never came back, with no
// webhook and no provider ids on our side (a form posted to the provider: its
// transaction id is our reference), is found by reconciliation through the
// reference, and the payment succeeds even though it expired meanwhile.
func TestReconciliationFindsAPaidCheckoutByItsReference(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR"})
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.core.CreateAttempt(ctx, res.Payment.ID, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.core.RecordCheckoutAction(ctx, a.ID, "", []byte(`{"kind":"form_post","url":"https://secure.payu.example/_payment","fields":{"txnid":"`+a.ProviderRef+`"}}`),
			e.clk.Now().Add(15*time.Minute)); err != nil {
			t.Fatal(err)
		}
		// The payment's time runs out with nothing heard from the provider.
		e.clk.Add(2 * time.Hour)
		if _, err := e.core.ExpireDue(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if p, _ := e.core.GetPaymentByID(ctx, res.Payment.ID); p.Status != "expired" {
			t.Fatalf("setup: payment is %s", p.Status)
		}
		// The provider has it as paid, under our reference as its transaction id.
		e.provider(a.ProviderRef, paid("", "pay_lost", 50000, "INR"))
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		if run.Fixed != 1 {
			t.Fatalf("run %+v items %+v", run, e.items(run.ID))
		}
		p, _ := e.core.GetPaymentByID(ctx, res.Payment.ID)
		got, _ := e.core.GetAttempt(ctx, a.ID)
		if p.Status != "succeeded" || p.AmountReceived != 50000 || got.Status != "captured" || got.ProviderPaymentID != "pay_lost" {
			t.Errorf("payment %+v attempt %+v", p, got)
		}
	})
}
