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

// The whole merchant-triggered cycle on a month-end anchor: activation charges
// period 0, the notice goes 48h ahead, the charge runs on the (clamped)
// boundary with the notice's provider_ref, the renewal Payment is invoiced.
func TestRenewalLifecycleMonthEndClampNoticeChargeInvoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0) // 31 Jan 2027, 09:30 IST
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		if sub.Status != "active" {
			t.Fatalf("status = %s", sub.Status)
		}
		e.runJobs() // period 0 is charged right away
		sub = e.sub(sub.ID)
		if e.r.chargeCount() != 1 || sub.RenewalsDone != 1 || !sub.CurrentPeriodStart.Equal(t0) || !sub.CurrentPeriodEnd.Equal(utc(2027, 2, 28, 4, 0)) {
			t.Fatalf("after period 0: charges=%d sub=%+v", e.r.chargeCount(), sub)
		}
		pays := e.renewalPayments(sub.ID)
		if len(pays) != 1 || pays[0].Kind != "renewal" || pays[0].Status != "succeeded" || pays[0].Amount != 49900 || pays[0].SubscriptionID != sub.ID {
			t.Fatalf("payments = %+v", pays)
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
		if e.r.chargeCount() != 1 {
			t.Fatal("charged at notice time")
		}

		// The charge runs on 28 Feb (31 Jan clamped) with the notice's reference.
		e.advanceTo(utc(2027, 2, 28, 3, 59))
		if e.r.chargeCount() != 1 {
			t.Fatal("charged early")
		}
		e.advanceTo(utc(2027, 2, 28, 4, 0))
		if e.r.chargeCount() != 2 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
		if e.r.charges[1].ProviderRef != notice.ProviderRef || e.r.charges[1].Amount != 49900 {
			t.Errorf("charge %+v does not match notice %+v", e.r.charges[1], notice)
		}
		sub = e.sub(sub.ID)
		if sub.RenewalsDone != 2 || !sub.CurrentPeriodStart.Equal(utc(2027, 2, 28, 4, 0)) || !sub.CurrentPeriodEnd.Equal(utc(2027, 3, 31, 4, 0)) {
			t.Errorf("after period 1: %+v (31 Jan + 2 months must be 31 Mar, not drift to 28 Mar)", sub)
		}

		// The renewal payment went through the ordinary machinery: invoice issued.
		pays = e.renewalPayments(sub.ID)
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
		e.runJobs()                                   // period 0 is charged; period 1's notice fails
		if e.r.chargeCount() != 1 || len(e.r.notices) != 0 {
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
		if e.r.chargeCount() != 1 {
			t.Fatalf("charged %dh after the notice", 12)
		}
		cy := e.cycles(sub.ID)
		if len(cy) < 2 || cy[1].State != "scheduled" {
			t.Fatalf("cycles = %+v", cy)
		}
		if got := e.pendingJobs("subscription.charge_renewal"); len(got) != 1 || !got[0].Equal(t0.Add(36*hour)) {
			t.Fatalf("rescheduled charge = %v, want notice + 24h = %v", got, t0.Add(36*hour))
		}
		e.advanceTo(t0.Add(36*hour - time.Minute))
		if e.r.chargeCount() != 1 {
			t.Fatal("charged before the 24h gap passed")
		}
		e.advanceTo(t0.Add(36 * hour))
		if e.r.chargeCount() != 2 {
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
		if e.r.chargeCount() != 1 {
			t.Fatalf("charged without a notice: %d", e.r.chargeCount())
		}
		e.drive(23*hour, hour)
		if e.r.chargeCount() != 1 {
			t.Fatalf("charged inside the 24h after the late notice: %d", e.r.chargeCount())
		}
		e.drive(2*hour, hour)
		if e.r.chargeCount() != 2 || e.sub(sub.ID).RenewalsDone != 2 {
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
		c := e.mustCreate(nil) // UPI
		e.activateMandate(c)
		if got := e.pendingJobs("subscription.charge_renewal"); len(got) != 1 || !got[0].Equal(utc(2027, 1, 15, 7, 30)) {
			t.Fatalf("UPI charge job = %v, want 13:00 IST (07:30 UTC)", got)
		}
		e.runJobs()
		if e.r.chargeCount() != 0 {
			t.Fatal("UPI debit executed during the peak window")
		}
		e.advanceTo(utc(2027, 1, 15, 7, 29))
		if e.r.chargeCount() != 0 {
			t.Fatal("UPI debit executed before the window ended")
		}
		e.advanceTo(utc(2027, 1, 15, 7, 30))
		if e.r.chargeCount() != 1 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}

		// A card mandate in the same minute is charged immediately.
		c2 := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card" })
		e.setNow(utc(2027, 1, 16, 6, 0))
		before := e.r.chargeCount()
		e.activateMandate(c2)
		e.runJobs()
		if e.r.chargeCount() != before+1 {
			t.Errorf("card charge was snapped: %d", e.r.chargeCount()-before)
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
		upi := e.mustCreate(nil)
		e.activateMandate(upi)
		e.drive(20*hour, 30*time.Minute)
		if got := e.r.chargeCount(); got != 4 {
			t.Fatalf("UPI charges = %d, want 1 + 3 retries", got)
		}
		if s := e.sub(upi.Subscription.ID); s.Status != "canceled" {
			t.Errorf("status = %s", s.Status)
		}

		e.r.mu.Lock()
		e.r.charges = nil
		e.r.mu.Unlock()
		card := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card" })
		e.activateMandate(card)
		e.drive(20*hour, 30*time.Minute)
		if got := e.r.chargeCount(); got != 6 {
			t.Fatalf("card charges = %d, want 1 + 5 retries", got)
		}
	})
}

func TestDunningGoesPastDueThenCanceled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.chargeFn = failing("card_declined")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card" })
		sub := e.activateMandate(c)
		e.runJobs()
		sub = e.sub(sub.ID)
		if sub.Status != "past_due" || sub.DunningAttempt != 1 || !sub.NextRetryAt.Equal(t0.Add(24*hour)) {
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
		// The failed payment stays failed; nothing was invoiced.
		for _, p := range e.renewalPayments(sub.ID) {
			if p.Status != "failed" {
				t.Errorf("payment %s is %s", p.ID, p.Status)
			}
		}
	})
}

func TestDunningKeepPastDue(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.setSetting("dunning_final_action", `"keep_past_due"`)
		e.setSetting("dunning_schedule", "[1]")
		e.r.chargeFn = failing("card_declined")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card" })
		sub := e.activateMandate(c)
		e.drive(6*hour, 30*time.Minute)
		if s := e.sub(sub.ID); s.Status != "past_due" || e.r.chargeCount() != 2 {
			t.Errorf("status=%s charges=%d", s.Status, e.r.chargeCount())
		}
		// Later success returns it to active.
		e.r.mu.Lock()
		e.r.chargeFn = nil
		e.r.mu.Unlock()
	})
}

func TestEachRetryNeedsItsOwnNoticeWhenTheProviderWantsOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		e.r.perAttempt = true
		e.r.chargeFn = failing("card_declined")
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card" })
		e.activateMandate(c)
		e.runJobs() // period 0 fails, retry 1 scheduled for +24h with a fresh notice
		if len(e.r.notices) != 1 {
			t.Fatalf("notices after the first failure = %d", len(e.r.notices))
		}
		e.drive(24*hour, hour)
		if e.r.chargeCount() != 2 {
			t.Fatalf("charges = %d", e.r.chargeCount())
		}
		// The retry used the attempt the notice announced.
		if e.r.charges[1].ProviderRef != e.r.notices[0].ProviderRef {
			t.Errorf("retry charge %s vs notice %s", e.r.charges[1].ProviderRef, e.r.notices[0].ProviderRef)
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
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		// A second, differently keyed job for the same cycle: both are claimed
		// by the same RunOnce and run in parallel.
		for i := 0; i < 3; i++ {
			err := db.Write(ctx, func(tx store.Tx) error {
				return e.q.Enqueue(ctx, tx, jobs.Job{Kind: "subscription.charge_renewal", RunAt: t0,
					Payload:   map[string]any{"subscription_id": sub.ID, "period": 0},
					DedupeKey: "dup-" + string(rune('a'+i))})
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		e.runJobs()
		if got := e.r.chargeCount(); got != 1 {
			t.Fatalf("charges = %d, want exactly 1", got)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 || pays[0].Status != "succeeded" {
			t.Fatalf("payments = %+v", pays)
		}
		if s := e.sub(sub.ID); s.RenewalsDone != 1 {
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
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		e.runJobs()
		if cy := e.cycles(sub.ID); len(cy) != 1 || cy[0].State != "awaiting" || e.sub(sub.ID).RenewalsDone != 0 {
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
		if got.RenewalsDone != 1 || got.Status != "active" {
			t.Fatalf("sub = %+v", got)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 || pays[0].Status != "succeeded" {
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
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = "card" })
		sub := e.activateMandate(c)
		e.runJobs()
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
		c := e.mustCreate(nil)
		sub := e.activateMandate(c)
		e.runJobs() // fails with a temporary error: the job is retried later
		if e.sub(sub.ID).RenewalsDone != 0 {
			t.Fatal("renewal booked after a temporary error")
		}
		e.drive(2*hour, 10*time.Minute)
		if e.r.chargeCount() != 2 || e.sub(sub.ID).RenewalsDone != 1 {
			t.Fatalf("charges=%d sub=%+v", e.r.chargeCount(), e.sub(sub.ID))
		}
		if e.r.charges[0].ProviderRef != e.r.charges[1].ProviderRef {
			t.Errorf("retry used a new reference: %s vs %s", e.r.charges[1].ProviderRef, e.r.charges[0].ProviderRef)
		}
		if pays := e.renewalPayments(sub.ID); len(pays) != 1 {
			t.Errorf("payments = %d", len(pays))
		}
	})
}
