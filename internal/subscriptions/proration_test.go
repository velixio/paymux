package subscriptions_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

// The base fixture: 49,900 a month from t0 = 2027-01-31 04:00, the first period
// (28 days) already paid by the registration transaction, so the next renewal is
// debited on 2027-02-28 04:00 after a notice two days earlier.
var (
	periodEnd = utc(2027, 2, 28, 4, 0)
	day7      = t0.AddDate(0, 0, 7)
)

func lines(t *testing.T, e *env, paymentID string) []invoice.Line {
	t.Helper()
	var out []invoice.Line
	var total, taxable, cgst, sgst int64
	_ = e.db.Read(ctx, func(q store.Querier) error {
		invs, err := store.InvoicesForPayment(ctx, q, paymentID)
		if err != nil || len(invs) != 1 {
			t.Fatalf("invoices of %s: %v %v", paymentID, invs, err)
		}
		if err := json.Unmarshal(invs[0].Lines, &out); err != nil {
			t.Fatal(err)
		}
		total, taxable, cgst, sgst = invs[0].Total, invs[0].Taxable, invs[0].CGST, invs[0].SGST
		return nil
	})
	var lt, lc, ls int64
	for _, l := range out {
		lt, lc, ls = lt+l.Taxable, lc+l.CGST, ls+l.SGST
	}
	if lt != taxable || lc != cgst || ls != sgst {
		t.Errorf("the lines %+v do not add up to the invoice (%d/%d/%d vs %d/%d/%d, total %d)", out, lt, lc, ls, taxable, cgst, sgst, total)
	}
	return out
}

func update(t *testing.T, e *env, id string, amount int64, mode subscriptions.Proration) subscriptions.UpdateResult {
	t.Helper()
	res, err := e.subs.Update(ctx, e.scope, id, subscriptions.UpdateInput{Amount: amount, Proration: mode})
	if err != nil {
		t.Fatalf("update to %d (%s): %v", amount, mode, err)
	}
	return res
}

func TestProrationNextInvoiceAddsTheUpgradeDifferenceToTheNextRenewal(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)

		res := update(t, e, sub.ID, 60000, subscriptions.ProrationNextInvoice)
		want := billing.Prorate(49900, 60000, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, day7)
		if want != 7575 { // 10,100 x 21 of 28 days
			t.Fatalf("test arithmetic: %d", want)
		}
		o := res.Proration
		if o == nil || o.Mode != subscriptions.ProrationNextInvoice || o.Amount != want || o.ChargeAt != nil || !strings.Contains(o.Description, "21.0 of 28.0 days") {
			t.Fatalf("outcome = %+v", o)
		}
		if got := e.sub(sub.ID); got.ProrationAdjust != want || got.PendingAmount != 60000 || got.Amount != 49900 || got.BilledAmount != 60000 || len(got.ProrationLines) != 1 {
			t.Fatalf("sub = %+v", got)
		}
		if e.r.chargeCount() != 0 {
			t.Fatal("next_invoice charged something now")
		}

		e.advanceTo(periodEnd.Add(-2 * 24 * time.Hour))
		if len(e.r.notices) != 1 || e.r.notices[0].Amount != 60000+want {
			t.Fatalf("the notice must announce the real amount: %+v", e.r.notices)
		}
		renew(e, periodEnd)
		if e.r.chargeCount() != 1 || e.r.charges[0].Amount != 60000+want {
			t.Fatalf("charges = %+v", e.r.charges)
		}
		got := e.sub(sub.ID)
		if got.Amount != 60000 || got.PendingAmount != 0 || got.ProrationAdjust != 0 || len(got.ProrationLines) != 0 || got.BilledAmount != 0 || got.RenewalsDone != 2 {
			t.Fatalf("after the renewal: %+v", got)
		}
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 2 || pays[1].Amount != 60000+want || pays[1].Status != "succeeded" {
			t.Fatalf("payments = %+v", pays)
		}
		// The invoice shows the period's price and the proration as separate lines.
		ls := lines(t, e, pays[1].ID)
		if len(ls) != 2 || ls[1].Description != o.Description {
			t.Fatalf("lines = %+v", ls)
		}
		gross := func(l invoice.Line) int64 { return l.Taxable + l.CGST + l.SGST + l.IGST }
		if gross(ls[0]) != 60000 || gross(ls[1]) != want {
			t.Errorf("line totals %d and %d, want 60000 and %d", gross(ls[0]), gross(ls[1]), want)
		}
		// The renewal after that is the plain new amount.
		renew(e, utc(2027, 3, 31, 4, 0))
		if got := e.r.charges[len(e.r.charges)-1].Amount; got != 60000 {
			t.Errorf("the following renewal charged %d", got)
		}
		if cy := e.cycles(sub.ID)[1]; len(cy.Adjustments) != 1 || cy.Adjustments[0].Amount != want {
			t.Errorf("cycle adjustments = %+v", cy.Adjustments)
		}
	})
}

func TestProrationDowngradeCreditsTheNextRenewalAndNeverRefunds(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)
		for _, mode := range []subscriptions.Proration{subscriptions.ProrationNextInvoice} {
			res := update(t, e, sub.ID, 39900, mode)
			if o := res.Proration; o == nil || o.Amount != -7500 || o.Mode != subscriptions.ProrationNextInvoice { // -10,000 x 21/28
				t.Fatalf("outcome = %+v", res.Proration)
			}
		}
		renew(e, periodEnd)
		if e.r.chargeCount() != 1 || e.r.charges[0].Amount != 39900-7500 {
			t.Fatalf("charges = %+v", e.r.charges)
		}
		pays := e.renewalPayments(sub.ID)
		ls := lines(t, e, pays[1].ID)
		if len(ls) != 2 || ls[1].Taxable+ls[1].CGST+ls[1].SGST+ls[1].IGST != -7500 {
			t.Fatalf("the credit must be a negative line: %+v", ls)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 0 {
			t.Error("a downgrade refunded money")
		}
		if got := e.sub(sub.ID); got.Amount != 39900 || got.ProrationAdjust != 0 {
			t.Errorf("sub = %+v", got)
		}
	})
}

func TestProrationCreditLargerThanTheRenewalCarriesOver(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(t0.AddDate(0, 0, 1))
		// A drop to 1,000 a month with 27 of 28 days left: a credit of about 47,000.
		update(t, e, sub.ID, 1000, subscriptions.ProrationNextInvoice)
		credit := e.sub(sub.ID).ProrationAdjust
		if credit > -40000 {
			t.Fatalf("credit = %d", credit)
		}
		renew(e, periodEnd)
		first := e.r.charges[0].Amount
		if first != 1 { // the credit exceeds the 1,000 renewal: one minor unit is still charged
			t.Fatalf("first renewal charged %d", first)
		}
		left := e.sub(sub.ID).ProrationAdjust
		if left != credit+999 || left >= 0 {
			t.Fatalf("carried credit = %d (started at %d)", left, credit)
		}
		// It keeps reducing renewals until it is used up, and is never paid out.
		renew(e, utc(2027, 3, 31, 4, 0))
		if got := e.r.charges[len(e.r.charges)-1].Amount; got != 1 {
			t.Errorf("second renewal charged %d", got)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 0 {
			t.Error("a credit was refunded")
		}
	})
}

func TestProrationImmediateChargesAfterTheNoticeGap(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)
		before := len(e.r.notices)

		res := update(t, e, sub.ID, 60000, subscriptions.ProrationImmediate)
		o := res.Proration
		if o == nil || o.Mode != subscriptions.ProrationImmediate || o.Amount != 7575 || o.ChargeAt == nil || o.Reason != "" {
			t.Fatalf("outcome = %+v", o)
		}
		if o.ChargeAt.Before(day7.Add(24 * time.Hour)) {
			t.Fatalf("the debit is announced for %v, less than 24h after the notice", o.ChargeAt)
		}
		e.runJobs()
		if len(e.r.notices) != before+1 || e.r.notices[before].Amount != 7575 {
			t.Fatalf("the notice for the prorated debit: %+v", e.r.notices)
		}
		if e.r.chargeCount() != 0 {
			t.Fatal("charged before the 24h gap")
		}
		e.advanceTo(day7.Add(23 * time.Hour))
		if e.r.chargeCount() != 0 {
			t.Fatal("charged inside the 24h gap")
		}
		e.advanceTo(day7.Add(36 * time.Hour))
		if e.r.chargeCount() != 1 || e.r.charges[0].Amount != 7575 {
			t.Fatalf("charges = %+v", e.r.charges)
		}

		// It is a charge for the current period, not a renewal.
		got := e.sub(sub.ID)
		if got.RenewalsDone != 1 || got.Status != "active" || !got.CurrentPeriodEnd.Equal(periodEnd) || got.Amount != 49900 || got.ProrationAdjust != 0 || got.PendingAmount != 60000 {
			t.Fatalf("sub = %+v", got)
		}
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 2 || pays[1].Amount != 7575 || pays[1].Status != "succeeded" || pays[1].Description != "Plan change proration" {
			t.Fatalf("payments = %+v", pays)
		}
		ls := lines(t, e, pays[1].ID)
		if len(ls) != 1 || !strings.HasPrefix(ls[0].Description, "Proration: plan change") || ls[0].Taxable+ls[0].CGST+ls[0].SGST+ls[0].IGST != 7575 {
			t.Fatalf("lines = %+v", ls)
		}
		if len(e.eventsOf("subscription.renewed")) != 1 {
			t.Errorf("a prorated charge was reported as a renewal: %v", e.eventTypes())
		}

		// The renewal that follows is the plain new amount, with its own notice.
		renew(e, periodEnd)
		if n := len(e.r.charges); n != 2 || e.r.charges[1].Amount != 60000 {
			t.Fatalf("renewal charges = %+v", e.r.charges)
		}
		if got := e.sub(sub.ID); got.Amount != 60000 || got.RenewalsDone != 2 || got.BilledAmount != 0 {
			t.Errorf("after the renewal: %+v", got)
		}
	})
}

func TestProrationImmediateDowngradeIsACreditNotACharge(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)
		res := update(t, e, sub.ID, 39900, subscriptions.ProrationImmediate)
		o := res.Proration
		if o.Mode != subscriptions.ProrationNextInvoice || o.Amount != -7500 || o.Reason != "credit_applies_to_the_next_renewal" || o.ChargeAt != nil {
			t.Fatalf("outcome = %+v", o)
		}
		e.advanceTo(day7.AddDate(0, 0, 3))
		if e.r.chargeCount() != 0 || e.sub(sub.ID).ProrationAdjust != -7500 {
			t.Fatalf("charges=%d sub=%+v", e.r.chargeCount(), e.sub(sub.ID))
		}
	})
}

func TestProrationImmediateFallsBackWhenNoNoticeCanBeSent(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)
		update(t, e, sub.ID, 100000, subscriptions.ProrationNone) // above the 74,850 maximum: a re-authorization starts
		if !e.sub(sub.ID).RequiresReauthorization {
			t.Fatal("setup: no re-authorization")
		}
		res := update(t, e, sub.ID, 60000, subscriptions.ProrationImmediate)
		o := res.Proration
		if o.Mode != subscriptions.ProrationNextInvoice || o.Reason != "mandate_needs_reauthorization" || o.Amount != 7575 || o.ChargeAt != nil {
			t.Fatalf("outcome = %+v", o)
		}
		if got := e.sub(sub.ID); got.ProrationAdjust != 7575 {
			t.Errorf("the difference must wait for the next renewal: %+v", got)
		}
	})
}

func TestProrationFailedChargeMovesToTheNextRenewal(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)
		update(t, e, sub.ID, 60000, subscriptions.ProrationImmediate)
		e.runJobs() // the notice goes out now
		e.r.chargeFn = failing("INSUFFICIENT_FUNDS")
		e.advanceTo(day7.Add(36 * time.Hour))
		if e.r.chargeCount() != 1 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
		got := e.sub(sub.ID)
		if got.Status != "active" || got.ProrationAdjust != 7575 || got.DunningAttempt != 0 || len(e.eventsOf("subscription.past_due")) != 0 {
			t.Fatalf("a failed prorated charge must not start dunning: %+v events=%v", got, e.eventTypes())
		}
		// No retries of the prorated charge...
		e.advanceTo(day7.AddDate(0, 0, 6))
		if e.r.chargeCount() != 1 {
			t.Errorf("the prorated charge was retried (%d charges)", e.r.chargeCount())
		}
		// ...the renewal collects it.
		e.r.chargeFn = nil
		renew(e, periodEnd)
		if last := e.r.charges[len(e.r.charges)-1].Amount; last != 60000+7575 {
			t.Fatalf("renewal charged %d", last)
		}
		if got := e.sub(sub.ID); got.ProrationAdjust != 0 || got.Amount != 60000 {
			t.Errorf("after the renewal: %+v", got)
		}
	})
}

func TestProrationAcrossSeveralChangesInOnePeriod(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		e.advanceTo(day7)
		update(t, e, sub.ID, 60000, subscriptions.ProrationNextInvoice) // +7575
		e.advanceTo(t0.AddDate(0, 0, 14))
		// Back to the original price: measured against what the period is paid at now (60,000).
		res := update(t, e, sub.ID, 49900, subscriptions.ProrationNextInvoice)
		if res.Proration == nil || res.Proration.Amount != -5050 { // -10,100 x 14 of 28
			t.Fatalf("outcome = %+v", res.Proration)
		}
		got := e.sub(sub.ID)
		if got.ProrationAdjust != 7575-5050 || got.PendingAmount != 0 || got.Amount != 49900 || len(got.ProrationLines) != 2 {
			t.Fatalf("sub = %+v", got)
		}
		renew(e, periodEnd)
		if e.r.chargeCount() != 1 || e.r.charges[0].Amount != 49900+2525 {
			t.Fatalf("charges = %+v", e.r.charges)
		}
	})
}

func TestProrationRulesAndRefusals(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := active(t, e, nil)
		if _, err := e.subs.Update(ctx, e.scope, sub.ID, subscriptions.UpdateInput{Amount: 60000, Proration: "sometimes"}); !errors.Is(err, subscriptions.ErrInvalid) {
			t.Errorf("unknown mode: %v", err)
		}
		// None is the default and leaves everything as it was.
		res := update(t, e, sub.ID, 60000, "")
		if res.Proration != nil || e.sub(sub.ID).ProrationAdjust != 0 || e.sub(sub.ID).BilledAmount != 0 {
			t.Errorf("default mode prorated: %+v %+v", res.Proration, e.sub(sub.ID))
		}
		// The very end of the period has nothing left to prorate.
		e.advanceTo(periodEnd.Add(-time.Second))
		if res := update(t, e, sub.ID, 70000, subscriptions.ProrationNextInvoice); res.Proration.Amount != 0 && res.Proration.Mode != subscriptions.ProrationNone {
			t.Errorf("outcome = %+v", res.Proration)
		}
	})
}

// A provider-managed subscription is billed by the provider, which cannot be
// told to add a one-off difference.
func TestProrationIsRefusedForProviderManagedSubscriptions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		sub := managed(t, e)
		for _, n := range rzp(t, "event_subscription_activated") {
			if err := e.subs.HandleNotification(ctx, "pa_managed", n); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.sub(sub.ID); got.Status != "active" {
			t.Fatalf("setup: %+v", got)
		}
		for _, mode := range []subscriptions.Proration{subscriptions.ProrationNextInvoice, subscriptions.ProrationImmediate} {
			_, err := e.subs.Update(ctx, e.scope, sub.ID, subscriptions.UpdateInput{Amount: 59900, Proration: mode})
			if !errors.Is(err, subscriptions.ErrUnsupported) {
				t.Errorf("%s: %v", mode, err)
			}
		}
		if len(e.r.updated) != 0 {
			t.Errorf("the provider was told about a refused change: %v", e.r.updated)
		}
		// A plain change still goes to the provider.
		if _, err := e.subs.Update(ctx, e.scope, sub.ID, subscriptions.UpdateInput{Amount: 59900}); err != nil || len(e.r.updated) != 1 {
			t.Errorf("plain change: %v updated=%v", err, e.r.updated)
		}
	})
}

var _ = connector.MerchantTriggered

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// renew runs the clock through the notice (two days before) and the charge of
// the renewal due at at.
func renew(e *env, at time.Time) {
	e.t.Helper()
	e.advanceTo(at.Add(-48 * time.Hour))
	e.advanceTo(at)
}
