package subscriptions_test

import (
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

// AUDIT: /return/{pa}/{ref} for a mandate or subscription looks the object up
// by ref ALONE (GetMandateByRef / GetSubscriptionByProviderRef are not scoped
// to the account) and then asks the connector of the account named in the URL.
// A mandate created on pa_upi is activated by what pa_enach's connector says.
// In a multi-org hub, the account in the URL can be one the attacker controls
// (e.g. a remote connector pointing at their server), which then answers
// "active" for any mandate id. Expected: a return through another account is
// not handled (or changes nothing).
func TestAuditPayMandateReturnThroughAnotherAccountIsNotApplied(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.sub(c.Subscription.ID)
		m := e.mandate(sub.MandateID)
		if m.ProviderAccountID != "pa_upi" {
			t.Fatalf("mandate on %s", m.ProviderAccountID)
		}
		// What the OTHER account's connector answers (the env's connectors share one responder).
		// The setup payment names no amount: toSnapshot then books the full expected charge.
		e.r.mandates[m.ProviderRef] = connector.MandateSnapshot{ProviderMandateID: "pm_forged", Status: "active", RawStatus: "confirmed",
			SetupPayment: &connector.PaymentSnapshot{ProviderPaymentID: "pay_forged", Status: connector.PaymentSucceeded}}
		e.r.mandates["pm_forged"] = e.r.mandates[m.ProviderRef]
		_, handled, err := e.subs.ApplyReturn(ctx, "pa_enach", m.ProviderRef)
		got := e.mandate(m.ID)
		var cy store.Cycle
		var pay store.Payment
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			if cy, err = store.GetCycle(ctx, q, sub.ID, 0); err != nil {
				return err
			}
			a, err := store.GetAttempt(ctx, q, cy.AttemptID)
			if err != nil {
				return err
			}
			pay, err = store.GetPayment(ctx, q, a.PaymentID)
			return err
		})
		t.Logf("period-0 payment %s: status=%s amount_received=%d", pay.ID, pay.Status, pay.AmountReceived)
		if handled && err == nil && got.Status == "active" {
			t.Fatalf("a return through pa_enach activated a mandate of pa_upi: mandate=%+v sub=%s first payment=%s", got, e.sub(sub.ID).Status, pay.Status)
		}
	})
}
