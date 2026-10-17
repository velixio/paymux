package subscriptions_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

const hour = time.Hour

// Every test also checks, on exit, that no merchant-triggered debit ever ran
// without a pre-debit notice at least 24h before it (env_test.go).

// drive moves the clock forward in steps, running due jobs after each step.
func (e *env) drive(d, step time.Duration) {
	e.t.Helper()
	end := e.clock().Add(d)
	for e.clock().Before(end) {
		e.advanceTo(e.clock().Add(step))
	}
}

func failing(code string) func(connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	return func(connector.ChargeRequest) (connector.PaymentSnapshot, error) {
		return connector.PaymentSnapshot{Status: connector.PaymentFailed, ErrorCode: code, ErrorMessage: "declined"}, nil
	}
}

// Period 0 is the registration transaction itself (AFA, no separate debit);
// the invoice for it is issued from that payment. Only later periods are
// merchant-triggered, each after a notice.
func TestSetupSuccessCreatesThePeriodZeroPaymentAndInvoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		e.runJobs()
		if e.r.chargeCount() != 0 {
			t.Fatalf("the mandate setup was followed by a separate debit (%d)", e.r.chargeCount())
		}
		sub = e.sub(sub.ID)
		if sub.Status != "active" || sub.RenewalsDone != 1 || !sub.CurrentPeriodStart.Equal(t0) || !sub.CurrentPeriodEnd.Equal(utc(2027, 2, 28, 4, 0)) {
			t.Fatalf("sub = %+v", sub)
		}
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Kind != "renewal" || pays[0].Status != "succeeded" || pays[0].Amount != 49900 || pays[0].AmountReceived != 49900 {
			t.Fatalf("payments = %+v", pays)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			atts, _ := store.ListAttempts(ctx, q, pays[0].ID)
			if len(atts) != 1 || atts[0].Status != "captured" || atts[0].ProviderPaymentID != "setup_pm_"+sub.MandateID || atts[0].ProviderAccountID != "pa_upi" {
				t.Errorf("attempts = %+v", atts)
			}
			invs, _ := store.InvoicesForPayment(ctx, q, pays[0].ID)
			if len(invs) != 1 || invs[0].Total != 49900 || invs[0].Number != "INV/2627/000001" {
				t.Errorf("invoice = %+v", invs)
			}
			return nil
		})
		cy := e.cycles(sub.ID)
		if len(cy) != 2 || cy[0].PeriodIndex != 0 || cy[0].State != "succeeded" || cy[1].PeriodIndex != 1 || cy[1].State != "scheduled" {
			t.Errorf("cycles = %+v", cy)
		}
		// Re-delivering the confirmation changes nothing.
		e.activateMandateByID(e.mandate(sub.MandateID), "pm_"+sub.MandateID)
		e.runJobs()
		if len(e.renewalPayments(sub.ID)) != 1 || e.sub(sub.ID).RenewalsDone != 1 {
			t.Error("a repeated confirmation booked period 0 again")
		}
	})
}

// A provider that does not report the registration transaction: the first
// period is then debited like any other, after a notice.
func TestWithoutASetupPaymentPeriodZeroWaitsForANotice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		sub := e.activateMandateNoSetup(c)
		e.runJobs()
		if sub.Status != "active" || e.r.chargeCount() != 0 || len(e.r.notices) != 1 {
			t.Fatalf("status=%s charges=%d notices=%d", sub.Status, e.r.chargeCount(), len(e.r.notices))
		}
		e.advanceTo(t0.Add(23 * hour))
		if e.r.chargeCount() != 0 {
			t.Fatal("debited inside 24h of the notice")
		}
		e.advanceTo(t0.Add(24 * hour))
		if e.r.chargeCount() != 1 || e.sub(sub.ID).RenewalsDone != 1 {
			t.Fatalf("charges=%d sub=%+v", e.r.chargeCount(), e.sub(sub.ID))
		}
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Status != "succeeded" {
			t.Errorf("payments = %+v", pays)
		}
	})
}

// A trial registers with the verification amount; the first real debit is at
// the trial's end and has its notice like any other.
func TestTrialRegistersMinimallyAndChargesAtTrialEndWithNotice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 14 })
		co, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c))
		if err != nil || co.Kind != subscriptions.CheckoutAction {
			t.Fatalf("checkout: %+v %v", co, err)
		}
		if got := e.r.mandateReqs[len(e.r.mandateReqs)-1]; got.Amount != 100 {
			t.Errorf("trial registration amount = %d, want the connector's verification amount (100)", got.Amount)
		}
		sub := e.activateMandate(c)
		e.runJobs()
		if sub.Status != "trialing" || len(e.renewalPayments(sub.ID)) != 0 || e.r.chargeCount() != 0 {
			t.Fatalf("status=%s payments=%d", sub.Status, len(e.renewalPayments(sub.ID)))
		}
		e.advanceTo(t0.AddDate(0, 0, 12)) // 48h before the end of the trial
		if len(e.r.notices) != 1 || e.r.notices[0].DebitDate != t0.AddDate(0, 0, 14).UnixMilli() {
			t.Fatalf("notices = %+v", e.r.notices)
		}
		e.advanceTo(t0.AddDate(0, 0, 13))
		if e.r.chargeCount() != 0 {
			t.Fatal("charged during the trial")
		}
		e.advanceTo(t0.AddDate(0, 0, 14))
		got := e.sub(sub.ID)
		if got.Status != "active" || e.r.chargeCount() != 1 || got.RenewalsDone != 1 || e.r.charges[0].Amount != 49900 {
			t.Fatalf("after the trial: %+v charges=%d", got, e.r.chargeCount())
		}
	})
}

func tokenOf(c subscriptions.Created) string {
	u := c.CheckoutURL
	for i := len(u) - 1; i >= 0; i-- {
		if u[i] == '=' {
			return u[i+1:]
		}
	}
	return ""
}

// The whole merchant-triggered cycle on a month-end anchor: the notice goes 48h
// ahead, the charge runs on the (clamped) boundary with the notice's
// provider_ref, the renewal Payment is invoiced.
func TestRenewalLifecycleMonthEndClampNoticeChargeInvoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0) // 31 Jan 2027, 09:30 IST
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		e.runJobs()
		sub = e.sub(sub.ID)
		if sub.RenewalsDone != 1 || !sub.CurrentPeriodEnd.Equal(utc(2027, 2, 28, 4, 0)) {
			t.Fatalf("after period 0: %+v", sub)
		}

		// Nothing happens until 48h before the boundary.
		if got := e.pendingJobs("subscription.notify_predebit"); len(got) != 1 || !got[0].Equal(utc(2027, 2, 26, 4, 0)) {
			t.Fatalf("notify jobs = %v", got)
		}
		e.advanceTo(utc(2027, 2, 26, 3, 59))
		if len(e.r.notices) != 0 {
			t.Fatal("notice sent early")
		}
		e.advanceTo(utc(2027, 2, 26, 4, 0))
		if len(e.r.notices) != 1 || len(e.eventsOf("subscription.renewal_upcoming")) != 1 {
			t.Fatalf("notices=%d events=%v", len(e.r.notices), e.eventTypes())
		}
		notice := e.r.notices[0]
		if notice.Amount != 49900 || notice.Currency != "INR" || notice.DebitDate != utc(2027, 2, 28, 4, 0).UnixMilli() || notice.Mandate.ProviderMandateID == "" {
			t.Errorf("notice = %+v", notice)
		}
		if e.r.chargeCount() != 0 {
			t.Fatal("charged at notice time")
		}

		// The charge runs on 28 Feb (31 Jan clamped) with the notice's reference.
		e.advanceTo(utc(2027, 2, 28, 3, 59))
		if e.r.chargeCount() != 0 {
			t.Fatal("charged early")
		}
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		if e.r.chargeCount() != 1 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
		if e.r.charges[0].ProviderRef != notice.ProviderRef || e.r.charges[0].Amount != 49900 {
			t.Errorf("charge %+v does not match notice %+v", e.r.charges[0], notice)
		}
		sub = e.sub(sub.ID)
		if sub.RenewalsDone != 2 || !sub.CurrentPeriodStart.Equal(utc(2027, 2, 28, 4, 0)) || !sub.CurrentPeriodEnd.Equal(utc(2027, 3, 31, 4, 0)) {
			t.Errorf("after period 1: %+v (31 Jan + 2 months must be 31 Mar, not drift to 28 Mar)", sub)
		}

		// The renewal payment went through the ordinary machinery: invoice issued.
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 2 || pays[1].Status != "succeeded" {
			t.Fatalf("payments = %+v", pays)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			invs, _ := store.InvoicesForPayment(ctx, q, pays[1].ID)
			if len(invs) != 1 || invs[0].Total != 49900 || invs[0].Number != "INV/2627/000002" {
				t.Errorf("renewal invoice = %+v", invs)
			}
			return nil
		})
		if got := e.eventTypes(); !slices.Contains(got, "subscription.renewed") || !slices.Contains(got, "invoice.issued") {
			t.Errorf("events = %v", got)
		}

		// Third period: back to the 31st, and one more clamp after that.
		e.advanceTo(utc(2027, 3, 29, 4, 0))
		e.advanceTo(utc(2027, 3, 31, 4, 0))
		if sub = e.sub(sub.ID); sub.RenewalsDone != 3 || !sub.CurrentPeriodEnd.Equal(utc(2027, 4, 30, 4, 0)) {
			t.Errorf("after period 2: %+v", sub)
		}
	})
}

// The debit never runs within 24h of the notice: it is refused and rescheduled.
func TestChargeWithin24HoursOfTheNoticeIsRefusedAndRescheduled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		sub := e.activateMandate(c)
		e.r.noticeErr = errors.New("notice API down") // the notice cannot go out yet
		e.runJobs()                                   // period 0 is the registration; period 1's notice fails
		if e.r.chargeCount() != 0 || len(e.r.notices) != 0 {
			t.Fatalf("charges=%d notices=%d", e.r.chargeCount(), len(e.r.notices))
		}
		// Period 1 is due at t0+24h; its notice only goes out at t0+12h.
		e.r.mu.Lock()
		e.r.noticeErr = nil
		e.r.mu.Unlock()
		e.advanceTo(t0.Add(12 * hour)) // 16:00 UTC = 21:30 IST, outside the peaks
		if len(e.r.notices) != 1 {
			t.Fatalf("notices = %d", len(e.r.notices))
		}
		e.advanceTo(t0.Add(24 * hour)) // the charge is due, but only 12h after the notice
		if e.r.chargeCount() != 0 {
			t.Fatal("debited 12h after the notice")
		}
		cy := e.cycles(sub.ID)
		if len(cy) < 2 || cy[1].State != "scheduled" {
			t.Fatalf("cycles = %+v", cy)
		}
		if got := e.pendingJobs("subscription.charge_renewal"); len(got) != 1 || !got[0].Equal(t0.Add(36*hour)) {
			t.Fatalf("rescheduled charge = %v, want notice + 24h = %v", got, t0.Add(36*hour))
		}
		e.advanceTo(t0.Add(36*hour - time.Minute))
		if e.r.chargeCount() != 0 {
			t.Fatal("charged before the 24h gap passed")
		}
		e.advanceTo(t0.Add(36 * hour))
		if e.r.chargeCount() != 1 {
			t.Fatalf("charges = %d, want the rescheduled one", e.r.chargeCount())
		}
		if s := e.sub(sub.ID); s.RenewalsDone != 2 {
			t.Errorf("renewals = %d", s.RenewalsDone)
		}
	})
}

// A charge that finds no notice sent first asks for one and waits 24h.
func TestChargeWithoutAnyNoticeSendsOneAndWaits(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		sub := e.activateMandate(c)
		e.r.noticeErr = errors.New("notice API down")
		e.runJobs()
		e.drive(23*hour, hour) // the notice job keeps failing
		e.r.mu.Lock()
		e.r.noticeErr = nil
		e.r.mu.Unlock()
		e.advanceTo(t0.Add(24 * hour)) // the charge is due and finds no notice
		if e.r.chargeCount() != 0 {
			t.Fatalf("charged without a notice: %d", e.r.chargeCount())
		}
		e.drive(23*hour, hour)
		if e.r.chargeCount() != 0 {
			t.Fatalf("charged inside the 24h after the late notice: %d", e.r.chargeCount())
		}
		e.drive(2*hour, hour)
		if e.r.chargeCount() != 1 || e.sub(sub.ID).RenewalsDone != 2 {
			t.Fatalf("charges=%d notices=%d", e.r.chargeCount(), len(e.r.notices))
		}
	})
}

// UPI AutoPay does not execute in the NPCI peak windows: the charge is snapped
// to the end of the window; other methods are not.
func TestUPIChargesSnapOutsidePeakHours(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		peak := utc(2027, 1, 15, 6, 0) // 11:30 IST: inside 10:00-13:00
		e := newEnv(t, db, peak)
		upi := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		card := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day"; i.Method = "card" })
		e.activateMandate(upi)
		e.activateMandate(card)
		e.runJobs() // period 0 is the registration; the notices for period 1 go out now
		if len(e.r.notices) != 2 {
			t.Fatalf("notices = %d", len(e.r.notices))
		}
		// Both are due at 06:00 UTC tomorrow, but only the card one may run then.
		due := peak.Add(24 * hour)
		if got := e.pendingJobs("subscription.charge_renewal"); len(got) != 2 || !got[0].Equal(due) || !got[1].Equal(utc(2027, 1, 16, 7, 30)) {
			t.Fatalf("charge jobs = %v, want 06:00 (card) and 13:00 IST = 07:30 UTC (UPI)", got)
		}
		e.advanceTo(due)
		if e.r.chargeCount() != 1 || e.r.charges[0].Mandate.ProviderMandateID != "pm_"+e.sub(card.Subscription.ID).MandateID {
			t.Fatalf("charges at 06:00 = %+v (only the card mandate may be debited in the peak window)", e.r.charges)
		}
		e.advanceTo(utc(2027, 1, 16, 7, 29))
		if e.r.chargeCount() != 1 {
			t.Fatal("UPI debit executed before the window ended")
		}
		e.advanceTo(utc(2027, 1, 16, 7, 30))
		if e.r.chargeCount() != 2 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
	})
}

// UPI allows 1 attempt + 3 retries per cycle whatever dunning_schedule says;
// other methods use the whole schedule.
func TestUPIRetriesAreCappedAtThree(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.setSetting("dunning_schedule", "[1,2,3,4,5]")
		e.r.chargeFn = failing("insufficient_funds")
		upi := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		e.activateMandate(upi)
		e.drive(48*hour, 30*time.Minute)
		if got := e.r.chargeCount(); got != 4 {
			t.Fatalf("UPI charges = %d, want 1 + 3 retries", got)
		}
		if s := e.sub(upi.Subscription.ID); s.Status != "canceled" {
			t.Errorf("status = %s", s.Status)
		}

		e.r.mu.Lock()
		e.r.charges, e.r.chargeTimes = nil, nil
		e.r.mu.Unlock()
		card := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day"; i.Method = "card" })
		e.activateMandate(card)
		e.drive(48*hour, 30*time.Minute)
		if got := e.r.chargeCount(); got != 6 {
			t.Fatalf("card charges = %d, want 1 + 5 retries", got)
		}
	})
}

func TestDunningGoesPastDueThenCanceled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.chargeFn = failing("card_declined")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card"; i.Interval = "day" })
		sub := e.activateMandate(c)
		e.runJobs()
		e.advanceTo(t0.Add(24 * hour)) // period 1 is the first merchant-triggered debit
		sub = e.sub(sub.ID)
		if sub.Status != "past_due" || sub.DunningAttempt != 1 || !sub.NextRetryAt.Equal(t0.Add(48*hour)) {
			t.Fatalf("after the first failure: %+v", sub)
		}
		if len(e.eventsOf("subscription.past_due")) != 1 {
			t.Errorf("events = %v", e.eventTypes())
		}
		// Retries on +1d, +3d, +5d after each failure (default dunning_schedule).
		e.drive(24*hour, hour)
		if e.r.chargeCount() != 2 {
			t.Fatalf("charges at +1d = %d", e.r.chargeCount())
		}
		e.drive(72*hour, hour)
		e.drive(120*hour, hour)
		sub = e.sub(sub.ID)
		if e.r.chargeCount() != 4 || sub.Status != "canceled" || sub.CanceledAt.IsZero() {
			t.Fatalf("charges=%d sub=%+v", e.r.chargeCount(), sub)
		}
		if len(e.eventsOf("subscription.past_due")) != 1 || len(e.eventsOf("subscription.canceled")) != 1 {
			t.Errorf("events = %v", e.eventTypes())
		}
		if m := e.mandate(sub.MandateID); m.Status != "revoked" {
			t.Errorf("mandate = %s", m.Status)
		}
		if len(e.r.revoked) != 1 {
			t.Errorf("provider revocations = %v", e.r.revoked)
		}
		// Period 0 (the registration) succeeded; the failed renewals stay failed.
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 2 || pays[0].Status != "succeeded" || pays[1].Status != "failed" {
			t.Errorf("payments = %+v", pays)
		}
	})
}

func TestDunningKeepPastDue(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.setSetting("dunning_final_action", `"keep_past_due"`)
		e.setSetting("dunning_schedule", "[1]")
		e.r.chargeFn = failing("card_declined")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card"; i.Interval = "day" })
		sub := e.activateMandate(c)
		e.drive(30*hour, 30*time.Minute)
		if s := e.sub(sub.ID); s.Status != "past_due" || e.r.chargeCount() != 2 {
			t.Errorf("status=%s charges=%d", s.Status, e.r.chargeCount())
		}
	})
}

func TestEachRetryNeedsItsOwnNoticeWhenTheProviderWantsOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.perAttempt = true
		e.r.chargeFn = failing("card_declined")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card"; i.Interval = "day" })
		e.activateMandate(c)
		e.drive(26*hour, hour) // period 1's debit at +24h fails; its retry is scheduled for +48h
		if e.r.chargeCount() != 1 || len(e.r.notices) != 2 {
			t.Fatalf("charges=%d notices=%d (a fresh notice for the retry)", e.r.chargeCount(), len(e.r.notices))
		}
		e.drive(24*hour, hour)
		if e.r.chargeCount() != 2 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
		// Each debit used the attempt its own notice announced.
		if e.r.charges[0].ProviderRef != e.r.notices[0].ProviderRef || e.r.charges[1].ProviderRef != e.r.notices[1].ProviderRef {
			t.Errorf("charges %v vs notices %v", e.r.charges, e.r.notices)
		}
		if e.r.charges[0].ProviderRef == e.r.charges[1].ProviderRef {
			t.Error("a retry reused the failed attempt's reference")
		}
	})
}

// Two job runs for the same renewal at once never charge twice.
func TestRenewalIsNeverChargedTwiceUnderConcurrentJobs(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.chargeDelay = 150 * time.Millisecond // widen the race window
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		sub := e.activateMandate(c)
		e.runJobs() // the notice for period 1
		due := t0.Add(24 * hour)
		// Extra, differently keyed jobs for the same cycle: they are claimed by
		// the same RunOnce as the real one and run in parallel.
		for i := 0; i < 3; i++ {
			err := db.Write(ctx, func(tx store.Tx) error {
				return e.q.Enqueue(ctx, tx, jobs.Job{Kind: "subscription.charge_renewal", RunAt: due,
					Payload:   map[string]any{"subscription_id": sub.ID, "period": 1},
					DedupeKey: "dup-" + string(rune('a'+i))})
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		e.advanceTo(due)
		if got := e.r.chargeCount(); got != 1 {
			t.Fatalf("charges = %d, want exactly 1", got)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 2 || pays[1].Status != "succeeded" {
			t.Fatalf("payments = %+v", pays)
		}
		if s := e.sub(sub.ID); s.RenewalsDone != 2 {
			t.Errorf("renewals = %d", s.RenewalsDone)
		}
	})
}

// A debit the provider has not settled stays awaiting and is polled.
func TestPendingChargeIsPolledUntilSettled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.chargeFn = func(connector.ChargeRequest) (connector.PaymentSnapshot, error) {
			return connector.PaymentSnapshot{Status: connector.PaymentProcessing, ProviderPaymentID: "pp_wait"}, nil
		}
		var mu sync.Mutex
		pending := true
		e.r.payFn = func(ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
			mu.Lock()
			defer mu.Unlock()
			if pending {
				return connector.PaymentSnapshot{Status: connector.PaymentProcessing, ProviderPaymentID: "pp_wait"}, nil
			}
			return connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 49900, Currency: "INR", ProviderPaymentID: "pp_wait"}, nil
		}
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		sub := e.activateMandate(c)
		e.runJobs()
		e.advanceTo(t0.Add(24 * hour))
		if cy := e.cycles(sub.ID); len(cy) != 2 || cy[1].State != "awaiting" || e.sub(sub.ID).RenewalsDone != 1 {
			t.Fatalf("cycles = %+v", cy)
		}
		e.drive(2*hour, 30*time.Minute) // still processing: keeps polling, never recharges
		if e.r.chargeCount() != 1 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
		mu.Lock()
		pending = false
		mu.Unlock()
		e.drive(hour, 30*time.Minute)
		got := e.sub(sub.ID)
		if got.RenewalsDone != 2 || got.Status != "active" {
			t.Fatalf("sub = %+v", got)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 2 || pays[1].Status != "succeeded" {
			t.Errorf("payments = %+v", pays)
		}
	})
}

// A debit that turns out failed at the poll enters dunning.
func TestPendingChargeThatFailsEntersDunning(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.chargeFn = func(connector.ChargeRequest) (connector.PaymentSnapshot, error) {
			return connector.PaymentSnapshot{Status: connector.PaymentProcessing}, nil
		}
		e.r.payFn = func(connector.PaymentRef) (connector.PaymentSnapshot, error) {
			return connector.PaymentSnapshot{Status: connector.PaymentFailed, ErrorCode: "mandate_cancelled"}, nil
		}
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card"; i.Interval = "day" })
		sub := e.activateMandate(c)
		e.runJobs()
		e.advanceTo(t0.Add(24 * hour))
		e.drive(hour, 30*time.Minute)
		got := e.sub(sub.ID)
		if got.Status != "past_due" || got.DunningAttempt != 1 {
			t.Fatalf("sub = %+v", got)
		}
	})
}

func TestChargeTemporaryErrorIsRetriedWithTheSameReference(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		calls := 0
		e.r.chargeFn = func(req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
			calls++
			if calls == 1 {
				return connector.PaymentSnapshot{}, connector.ErrTemporary
			}
			return connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: req.Amount, Currency: "INR", ProviderPaymentID: "pp_ok"}, nil
		}
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Interval = "day" })
		sub := e.activateMandate(c)
		e.runJobs()
		e.advanceTo(t0.Add(24 * hour)) // fails with a temporary error: the job is retried later
		if e.sub(sub.ID).RenewalsDone != 1 {
			t.Fatal("renewal booked after a temporary error")
		}
		e.drive(2*hour, 10*time.Minute)
		if e.r.chargeCount() != 2 || e.sub(sub.ID).RenewalsDone != 2 {
			t.Fatalf("charges=%d sub=%+v", e.r.chargeCount(), e.sub(sub.ID))
		}
		if e.r.charges[0].ProviderRef != e.r.charges[1].ProviderRef {
			t.Errorf("retry used a new reference: %s vs %s", e.r.charges[1].ProviderRef, e.r.charges[0].ProviderRef)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 2 {
			t.Errorf("payments = %d", len(pays))
		}
	})
}
