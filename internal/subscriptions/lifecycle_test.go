package subscriptions_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

// active returns a charged, active UPI subscription (period 0 paid at t0).
func active(t *testing.T, e *env, mut func(*subscriptions.CreateInput)) store.Subscription {
	t.Helper()
	c := e.mustCreate(mut)
	sub := e.activateMandate(c)
	e.runJobs()
	return e.sub(sub.ID)
}

func TestCancelNowRevokesTheMandate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		got, err := e.subs.Cancel(ctx, e.scope, sub.ID, false)
		if err != nil || got.Status != "canceled" || got.CanceledAt.IsZero() {
			t.Fatalf("cancel: %+v %v", got, err)
		}
		if m := e.mandate(sub.MandateID); m.Status != "revoked" {
			t.Errorf("mandate = %s", m.Status)
		}
		e.runJobs()
		if len(e.r.revoked) != 1 {
			t.Errorf("provider revocations = %v", e.r.revoked)
		}
		if len(e.eventsOf("subscription.canceled")) != 1 || len(e.eventsOf("mandate.revoked")) != 1 {
			t.Errorf("events = %v", e.eventTypes())
		}
		// Canceling again is harmless; nothing is charged any more.
		if _, err := e.subs.Cancel(ctx, e.scope, sub.ID, false); err != nil {
			t.Errorf("second cancel: %v", err)
		}
		e.drive(70*24*hour, 24*hour)
		if e.r.chargeCount() != 1 {
			t.Errorf("charges after cancel = %d", e.r.chargeCount())
		}
		// Scoped to its app and mode.
		other := e.scope
		other.Mode = "test"
		if _, err := e.subs.Cancel(ctx, other, sub.ID, false); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Errorf("cross-mode cancel: %v", err)
		}
	})
}

func TestCancelAtPeriodEnd(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		got, err := e.subs.Cancel(ctx, e.scope, sub.ID, true)
		if err != nil || got.Status != "active" || !got.CancelAtPeriodEnd {
			t.Fatalf("cancel: %+v %v", got, err)
		}
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		if len(e.r.notices) != 0 {
			t.Error("announced a debit that will not happen")
		}
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		got = e.sub(sub.ID)
		if got.Status != "canceled" || e.r.chargeCount() != 1 {
			t.Fatalf("at the boundary: status=%s charges=%d", got.Status, e.r.chargeCount())
		}
		if ev := e.eventsOf("subscription.canceled"); len(ev) != 1 {
			t.Errorf("events = %v", e.eventTypes())
		}
		if m := e.mandate(sub.MandateID); m.Status != "revoked" {
			t.Errorf("mandate = %s", m.Status)
		}
	})
}

func TestCancelIncomplete(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		got, err := e.subs.Cancel(ctx, e.scope, c.Subscription.ID, false)
		if err != nil || got.Status != "canceled" {
			t.Fatalf("%+v %v", got, err)
		}
		if _, err := e.subs.UpdateAmount(ctx, e.scope, got.ID, 100); !errors.Is(err, subscriptions.ErrInvalidState) {
			t.Errorf("update on canceled: %v", err)
		}
	})
}

func TestTotalCountEndsTheSubscription(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, func(i *subscriptions.CreateInput) { i.TotalCount = 2; i.Interval = "week" })
		e.advanceTo(t0.AddDate(0, 0, 5))
		e.advanceTo(t0.AddDate(0, 0, 7))
		if got := e.sub(sub.ID); got.RenewalsDone != 2 || got.Status != "active" {
			t.Fatalf("after 2 periods: %+v", got)
		}
		e.advanceTo(t0.AddDate(0, 0, 12))
		e.advanceTo(t0.AddDate(0, 0, 14))
		got := e.sub(sub.ID)
		if got.Status != "ended" || e.r.chargeCount() != 2 || got.EndedAt.IsZero() {
			t.Fatalf("status=%s charges=%d", got.Status, e.r.chargeCount())
		}
		if len(e.eventsOf("subscription.ended")) != 1 {
			t.Errorf("events = %v", e.eventTypes())
		}
	})
}

func TestTrialChargesAtTrialEnd(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 14 })
		sub := e.activateMandate(c)
		e.runJobs()
		if sub.Status != "trialing" || e.r.chargeCount() != 0 {
			t.Fatalf("status=%s charges=%d", sub.Status, e.r.chargeCount())
		}
		e.advanceTo(t0.AddDate(0, 0, 13))
		if e.r.chargeCount() != 0 {
			t.Fatal("charged during the trial")
		}
		e.advanceTo(t0.AddDate(0, 0, 14))
		got := e.sub(sub.ID)
		if got.Status != "active" || e.r.chargeCount() != 1 || got.RenewalsDone != 1 {
			t.Fatalf("after the trial: %+v charges=%d", got, e.r.chargeCount())
		}
	})
}

func TestUpdateAmountAppliesFromTheNextPeriod(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		res, err := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 60000) // within the 74,850 max
		if err != nil || res.CheckoutURL != "" || res.Subscription.RequiresReauthorization || res.Subscription.PendingAmount != 60000 || res.Subscription.Amount != 49900 {
			t.Fatalf("update: %+v %v", res, err)
		}
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		if len(e.r.notices) != 1 || e.r.notices[0].Amount != 60000 {
			t.Fatalf("notice = %+v", e.r.notices)
		}
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		got := e.sub(sub.ID)
		if got.Amount != 60000 || got.PendingAmount != 0 || e.r.charges[1].Amount != 60000 {
			t.Errorf("sub=%+v charge=%+v", got, e.r.charges[1])
		}
		if pays := e.renewalPayments(sub.ID); pays[0].Amount != 49900 || pays[1].Amount != 60000 {
			t.Errorf("the old period must keep its price: %+v", pays)
		}
		// Back to the current amount clears a pending change.
		if _, err := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 70000); err != nil {
			t.Fatal(err)
		}
		if res, _ := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 60000); res.Subscription.PendingAmount != 0 {
			t.Errorf("pending = %d", res.Subscription.PendingAmount)
		}
		if _, err := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 0); !errors.Is(err, subscriptions.ErrInvalid) {
			t.Errorf("zero amount: %v", err)
		}
		// Above what any UPI/card mandate allows: refused.
		if _, err := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 2_000_000); !errors.Is(err, subscriptions.ErrAmountExceedsMandateLimit) {
			t.Errorf("over the limit: %v", err)
		}
	})
}

// The full re-authorization: a larger amount needs a larger mandate; the old
// mandate is replaced and revoked, and the renewal that could not be charged
// meanwhile goes through.
func TestReauthorizationFlow(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		oldMandate := e.mandate(sub.MandateID)

		res, err := e.subs.UpdateAmount(ctx, e.scope, sub.ID, 100000) // above the 74,850 max
		if err != nil {
			t.Fatal(err)
		}
		if !res.Subscription.RequiresReauthorization || !strings.Contains(res.CheckoutURL, "/s/"+sub.ID+"?t=") || res.Subscription.PendingAmount != 100000 {
			t.Fatalf("update: %+v url=%q", res.Subscription, res.CheckoutURL)
		}
		ev := e.eventsOf("subscription.reauthorization_required")
		if len(ev) != 1 {
			t.Fatalf("events = %v", e.eventTypes())
		}
		if d, _ := ev[0].Data.(interface{}); d == nil {
			t.Fatal("no event data")
		}
		got := e.sub(sub.ID)
		if got.PendingMandateID == "" || got.MandateID != oldMandate.ID {
			t.Fatalf("mandates: %+v", got)
		}
		pending := e.mandate(got.PendingMandateID)
		if pending.Status != "pending" || pending.MaxAmount != 150000 || pending.Method != "upi" {
			t.Errorf("pending mandate = %+v", pending)
		}

		// The customer opens the new link: a fresh mandate checkout is created.
		token := res.CheckoutURL[strings.Index(res.CheckoutURL, "t=")+2:]
		co, err := e.subs.OpenCheckout(ctx, sub.ID, token)
		if err != nil || co.Kind != subscriptions.CheckoutAction || co.Action.Kind != connector.ActionFormPost {
			t.Fatalf("checkout: %+v %v", co, err)
		}
		last := e.r.mandateReqs[len(e.r.mandateReqs)-1]
		if last.MandateID != pending.ID || last.MaxAmount != 150000 || last.ProviderRef != pending.ProviderRef {
			t.Errorf("mandate request = %+v", last)
		}
		// The old token no longer works.
		if _, err := e.subs.OpenCheckout(ctx, sub.ID, "stale"); !errors.Is(err, subscriptions.ErrNotFound) {
			t.Errorf("stale token: %v", err)
		}

		// Renewal time comes before the customer re-authorised: the 100,000
		// debit exceeds the old mandate, so it is not even attempted.
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		got = e.sub(sub.ID)
		if e.r.chargeCount() != 1 || got.Status != "past_due" {
			t.Fatalf("charges=%d status=%s", e.r.chargeCount(), got.Status)
		}

		// The customer authorises the bigger mandate: swap, revoke, resume.
		e.activateMandateByID(pending, "pm_new")
		e.runJobs()
		got = e.sub(sub.ID)
		if got.MandateID != pending.ID || got.PendingMandateID != "" || got.RequiresReauthorization {
			t.Fatalf("after swap: %+v", got)
		}
		if m := e.mandate(oldMandate.ID); m.Status != "revoked" {
			t.Errorf("old mandate = %s", m.Status)
		}
		if len(e.r.revoked) != 1 || e.r.revoked[0] != "pm_"+oldMandate.ID {
			t.Errorf("provider revocations = %v", e.r.revoked)
		}
		if e.r.chargeCount() != 2 || e.r.charges[1].Amount != 100000 || e.r.charges[1].Mandate.ProviderMandateID != "pm_new" {
			t.Fatalf("resumed charge = %+v", e.r.charges)
		}
		if got.Status != "active" || got.Amount != 100000 || got.RenewalsDone != 2 {
			t.Errorf("sub = %+v", got)
		}
	})
}

func TestUpdatePaymentMethod(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		old := sub.MandateID
		res, err := e.subs.UpdatePaymentMethod(ctx, e.scope, sub.ID, "card")
		if err != nil || !strings.Contains(res.CheckoutURL, "/s/"+sub.ID+"?t=") {
			t.Fatalf("%+v %v", res, err)
		}
		got := e.sub(sub.ID)
		if got.PendingMandateID == "" || got.RequiresReauthorization || got.MandateID != old {
			t.Fatalf("sub = %+v", got)
		}
		pm := e.mandate(got.PendingMandateID)
		if pm.Method != "card" || pm.MaxAmount != 74850 {
			t.Errorf("pending mandate = %+v", pm)
		}
		// Until the new one is active the old keeps working; then it is replaced.
		e.activateMandateByID(pm, "pm_card")
		e.runJobs()
		got = e.sub(sub.ID)
		if got.MandateID != pm.ID || e.mandate(old).Status != "revoked" || len(e.r.revoked) != 1 {
			t.Errorf("after swap: %+v revoked=%v", got, e.r.revoked)
		}
		// Provider-managed subscriptions cannot do this through Paymux.
		e.routeTo("razorpay")
		mc := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "" })
		e.setSubStatus(mc.Subscription.ID, "active")
		if _, err := e.subs.UpdatePaymentMethod(ctx, e.scope, mc.Subscription.ID, ""); !errors.Is(err, subscriptions.ErrUnsupported) {
			t.Errorf("managed payment method: %v", err)
		}
	})
}

func (e *env) setSubStatus(id, status string) {
	e.t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE subscriptions SET status = ? WHERE id = ?", status, id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// A customer revoking the mandate in their UPI app: the next renewal fails
// straight into dunning with reauthorization_required, and a new mandate
// resumes it.
func TestMandateRevokedByTheCustomer(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		m := e.mandate(sub.MandateID)
		err := e.subs.HandleNotification(ctx, "pa_upi", connector.Notification{Kind: connector.NotifyMandate, EventID: "rv1",
			Mandate: &connector.MandateSnapshot{ProviderMandateID: m.ProviderMandateID, Status: "revoked", RawStatus: "cancelled"}})
		if err != nil {
			t.Fatal(err)
		}
		got := e.sub(sub.ID)
		if e.mandate(m.ID).Status != "revoked" || !got.RequiresReauthorization || got.PendingMandateID == "" || len(e.eventsOf("mandate.revoked")) != 1 {
			t.Fatalf("sub=%+v events=%v", got, e.eventTypes())
		}
		if ev := e.eventsOf("subscription.reauthorization_required"); len(ev) != 1 {
			t.Fatalf("events = %v", e.eventTypes())
		}
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		got = e.sub(sub.ID)
		if got.Status != "past_due" || e.r.chargeCount() != 1 {
			t.Fatalf("status=%s charges=%d (a revoked mandate must not be debited)", got.Status, e.r.chargeCount())
		}
		e.activateMandateByID(e.mandate(got.PendingMandateID), "pm_again")
		e.runJobs()
		got = e.sub(sub.ID)
		if got.Status != "active" || e.r.chargeCount() != 2 || got.RequiresReauthorization {
			t.Errorf("after re-authorization: %+v charges=%d", got, e.r.chargeCount())
		}
	})
}

// Thirty days before a mandate expires, re-authorization is requested.
func TestMandateExpiryWarning(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.setSetting("mandate_validity", "1")
		sub := active(t, e, func(i *subscriptions.CreateInput) { i.Interval = "year" })
		m := e.mandate(sub.MandateID)
		warn := m.ValidUntil.AddDate(0, 0, -30)
		e.advanceTo(warn.Add(-time.Minute))
		if len(e.eventsOf("subscription.reauthorization_required")) != 0 {
			t.Fatal("warned early")
		}
		e.advanceTo(warn)
		ev := e.eventsOf("subscription.reauthorization_required")
		got := e.sub(sub.ID)
		if len(ev) != 1 || !got.RequiresReauthorization || got.PendingMandateID == "" {
			t.Fatalf("events=%v sub=%+v", e.eventTypes(), got)
		}
		// Still billable until it actually expires.
		if got.Status != "active" {
			t.Errorf("status = %s", got.Status)
		}
	})
}

func TestMandateReturnActivatesTheSubscription(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.sub(c.Subscription.ID)
		m := e.mandate(sub.MandateID)
		// Not ours: a payment return.
		if _, handled, err := e.subs.ApplyReturn(ctx, "pa_upi", "NOSUCHREF"); handled || err != nil {
			t.Fatalf("unknown ref: handled=%v err=%v", handled, err)
		}
		// The provider has not confirmed yet: still incomplete.
		e.r.mandates[m.ProviderRef] = connector.MandateSnapshot{ProviderMandateID: "pm_ret", Status: "pending", RawStatus: "pending"}
		out, handled, err := e.subs.ApplyReturn(ctx, "pa_upi", m.ProviderRef)
		if err != nil || !handled || out.Status != "incomplete" {
			t.Fatalf("pending: %+v handled=%v err=%v", out, handled, err)
		}
		e.r.mandates["pm_ret"] = connector.MandateSnapshot{ProviderMandateID: "pm_ret", Status: "active", RawStatus: "confirmed"} // now known by its provider id
		out, _, err = e.subs.ApplyReturn(ctx, "pa_upi", m.ProviderRef)
		if err != nil || out.Status != "active" || out.SubscriptionID != sub.ID ||
			out.ReturnURL != "https://app.example.com/done?subscription_id="+sub.ID+"&status=active" {
			t.Fatalf("active: %+v err=%v", out, err)
		}
		if got := e.mandate(m.ID); got.ProviderMandateID != "pm_ret" || got.Status != "active" {
			t.Errorf("mandate = %+v", got)
		}
	})
}

var _ = audit.System
