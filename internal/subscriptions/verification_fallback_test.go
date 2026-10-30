package subscriptions_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/subscriptions"
	"github.com/velixio/paymux/pkg/connector"
)

// Whether PayU and Zoho reverse the Rs 1 verification debit themselves is not
// known (their connectors say they do). So Paymux checks: 72 hours after the
// debit, if the provider has not reversed it, Paymux refunds it explicitly.
// The refund is idempotent and never doubles one the provider or an operator
// made.

const fallbackJob = "subscription.verification_fallback"

// trialWithVerification registers a trial; the provider "reverses the debit
// itself" (the connector says so), and returns the verification payment.
func trialWithVerification(t *testing.T, e *env) store.Payment {
	t.Helper()
	c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 7 })
	sub := e.activateMandate(c)
	e.runJobs()
	for _, p := range e.renewalPayments(sub.ID) {
		if p.Kind == "mandate_setup" {
			return p
		}
	}
	t.Fatal("no verification payment")
	return store.Payment{}
}

func (e *env) providerShows(refunded int64) {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	e.r.payFn = func(connector.PaymentRef) (connector.PaymentSnapshot, error) {
		return connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 100, Currency: "INR", AmountRefunded: refunded}, nil
	}
}

func TestVerificationDebitIsRefundedAfter72HoursIfTheProviderDidNotReverseIt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		p := trialWithVerification(t, e)
		if len(e.refundsOf(p.ID)) != 0 {
			t.Fatal("refunded at once although the provider is believed to reverse it")
		}
		jobs := e.pendingJobs(fallbackJob)
		if len(jobs) != 1 || jobs[0].Before(t0.Add(71*time.Hour)) || jobs[0].After(t0.Add(73*time.Hour)) {
			t.Fatalf("fallback jobs = %v, want one about 72 hours out", jobs)
		}
		e.providerShows(0) // the provider has not reversed it
		e.advanceTo(t0.Add(71 * time.Hour))
		if len(e.refundsOf(p.ID)) != 0 {
			t.Fatal("refunded before the 72 hours")
		}
		e.advanceTo(t0.Add(73 * time.Hour))
		refunds := e.refundsOf(p.ID)
		if len(refunds) != 1 || refunds[0].InitiatedBy != "system" || refunds[0].Reason != "mandate_verification" || refunds[0].Amount != 100 {
			t.Fatalf("refunds = %+v", refunds)
		}
		// Running everything again, later, refunds nothing more.
		e.advanceTo(t0.Add(200 * time.Hour))
		if n := len(e.refundsOf(p.ID)); n != 1 {
			t.Errorf("refunds after more time = %d", n)
		}
	})
}

func TestVerificationFallbackLeavesAReversalTheProviderMade(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		p := trialWithVerification(t, e)
		e.providerShows(100) // the provider reversed it itself
		e.advanceTo(t0.Add(73 * time.Hour))
		if n := len(e.refundsOf(p.ID)); n != 0 {
			t.Errorf("refunded again although the provider had: %d refunds", n)
		}
	})
}

func TestVerificationFallbackLeavesARefundSomeoneAlreadyMade(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		p := trialWithVerification(t, e)
		e.providerShows(0)
		if _, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 100}); err != nil {
			t.Fatal(err)
		}
		e.advanceTo(t0.Add(73 * time.Hour))
		if n := len(e.refundsOf(p.ID)); n != 1 {
			t.Errorf("refunds = %d, want the one already there", n)
		}
	})
}

func TestVerificationFallbackCanBeSwitchedOff(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "verification_debit_fallback", json.RawMessage(`"never"`)); err != nil {
			t.Fatal(err)
		}
		p := trialWithVerification(t, e)
		if jobs := e.pendingJobs(fallbackJob); len(jobs) != 0 {
			t.Errorf("a fallback was scheduled with the setting off: %v", jobs)
		}
		e.providerShows(0)
		e.advanceTo(t0.Add(100 * time.Hour))
		if n := len(e.refundsOf(p.ID)); n != 0 {
			t.Errorf("refunds = %d with the fallback off", n)
		}
	})
}

// Registering the same mandate again schedules no second fallback.
func TestVerificationFallbackIsScheduledOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.TrialDays = 7 })
		sub := e.activateMandate(c)
		e.activateMandateByID(e.mandate(sub.MandateID), "pm_"+sub.MandateID)
		e.runJobs()
		if jobs := e.pendingJobs(fallbackJob); len(jobs) != 1 {
			t.Errorf("fallback jobs = %v", jobs)
		}
	})
}
