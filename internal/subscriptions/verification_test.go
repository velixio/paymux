package subscriptions_test

import (
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
)

func (e *env) refundsOf(paymentID string) []store.Refund {
	e.t.Helper()
	var out []store.Refund
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.ListRefunds(ctx, q, paymentID); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) invoicesOf(paymentID string) int {
	e.t.Helper()
	n := 0
	_ = e.db.Read(ctx, func(q store.Querier) error {
		invs, _ := store.InvoicesForPayment(ctx, q, paymentID)
		n = len(invs)
		return nil
	})
	return n
}

// A trial registers with the verification debit: recorded as a mandate_setup
// payment, never invoiced; the provider reverses it, so no refund is made.
func TestTrialVerificationDebitIsRecordedWithoutInvoiceOrRefund(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 7 })
		sub := e.activateMandate(c)
		e.runJobs()
		pays := e.renewalPayments(sub.ID)
		if sub.Status != "trialing" || len(pays) != 1 {
			t.Fatalf("status=%s payments=%+v", sub.Status, pays)
		}
		p := pays[0]
		if p.Kind != "mandate_setup" || p.Status != "succeeded" || p.Amount != 100 || p.AmountReceived != 100 || p.SubscriptionID != sub.ID {
			t.Fatalf("payment = %+v", p)
		}
		if e.invoicesOf(p.ID) != 0 {
			t.Error("a verification debit was invoiced")
		}
		if len(e.refundsOf(p.ID)) != 0 {
			t.Error("refunded although the provider reverses it")
		}
		if got := e.sub(sub.ID); got.RenewalsDone != 0 {
			t.Errorf("the verification debit counted as a renewal: %+v", got)
		}
		// Re-delivering the confirmation records nothing twice.
		e.activateMandateByID(e.mandate(sub.MandateID), "pm_"+sub.MandateID)
		e.runJobs()
		if n := len(e.renewalPayments(sub.ID)); n != 1 {
			t.Errorf("payments after a repeat = %d", n)
		}
		// The first real debit is later, with its notice, and is invoiced.
		e.advanceTo(t0.AddDate(0, 0, 5))
		e.advanceTo(t0.AddDate(0, 0, 7))
		pays = e.renewalPayments(sub.ID)
		if e.r.chargeCount() != 1 || len(pays) != 2 || pays[1].Kind != "renewal" || e.invoicesOf(pays[1].ID) != 1 {
			t.Fatalf("charges=%d payments=%+v", e.r.chargeCount(), pays)
		}
	})
}

// A provider that does not reverse it: refunded through the ordinary refund path.
func TestVerificationDebitIsRefundedWhenTheProviderDoesNotReverseIt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.noReverse = true
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 7 })
		sub := e.activateMandate(c)
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Kind != "mandate_setup" {
			t.Fatalf("payments = %+v", pays)
		}
		refunds := e.refundsOf(pays[0].ID)
		if len(refunds) != 1 {
			t.Fatalf("refunds = %+v", refunds)
		}
		r := refunds[0]
		if r.InitiatedBy != "system" || r.Reason != "mandate_verification" || r.Amount != 100 || r.Status != "pending" || r.ProviderRef == "" {
			t.Errorf("refund = %+v", r)
		}
		if e.invoicesOf(pays[0].ID) != 0 {
			t.Error("invoiced")
		}
		// Repeating the confirmation does not refund again.
		e.activateMandateByID(e.mandate(sub.MandateID), "pm_"+sub.MandateID)
		if n := len(e.refundsOf(pays[0].ID)); n != 1 {
			t.Errorf("refunds after a repeat = %d", n)
		}
	})
}

// A running subscription that registers a new mandate (new payment method,
// re-authorization) pays the verification debit too.
func TestReplacementMandateVerificationDebit(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.noReverse = true
		e.verifyOnSwap = true
		sub := active(t, e, nil)
		if _, err := e.subs.UpdatePaymentMethod(ctx, e.scope, sub.ID, "card"); err != nil {
			t.Fatal(err)
		}
		pending := e.mandate(e.sub(sub.ID).PendingMandateID)
		e.activateMandateByID(pending, "pm_card")
		e.runJobs()
		var setup []store.Payment
		for _, p := range e.renewalPayments(sub.ID) {
			if p.Kind == "mandate_setup" {
				setup = append(setup, p)
			}
		}
		if len(setup) != 1 || setup[0].Amount != 100 || setup[0].Status != "succeeded" {
			t.Fatalf("setup payments = %+v", setup)
		}
		if r := e.refundsOf(setup[0].ID); len(r) != 1 || r[0].Reason != "mandate_verification" {
			t.Errorf("refunds = %+v", r)
		}
		// Period 0 was not touched by it.
		if got := e.sub(sub.ID); got.RenewalsDone != 1 || got.MandateID != pending.ID {
			t.Errorf("sub = %+v", got)
		}
	})
}
