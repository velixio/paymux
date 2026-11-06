package core_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

func triesOf(t *testing.T, e *env, paymentID string) []store.ProviderPaymentAttempt {
	t.Helper()
	var out []store.ProviderPaymentAttempt
	if err := e.db.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.ListProviderPaymentAttempts(ctx, q, paymentID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApplyRecordsTryOnEveryPath(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		score := 62
		e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pp_1", Method: "card", RawStatus: "failed",
			FailureCode: "insufficient_funds", FailureMessage: "No funds.", ThreeDS: connector.ThreeDSPassed,
			MethodDetails: connector.PaymentMethodDetails{Card: &connector.CardDetails{Last4: "4242", Name: "S CUSTOMER"}},
			Risk:          connector.PaymentRisk{Score: &score, CVVCheck: connector.CheckMatch}})
		got := triesOf(t, e, p.ID)
		if len(got) != 1 {
			t.Fatalf("tries = %+v", got)
		}
		try := got[0]
		if try.Status != "failed" || try.FailureCode != "insufficient_funds" || try.FailureMessage != "No funds." || try.ThreeDS != "passed" || try.Method != "card" {
			t.Errorf("try = %+v", try)
		}
		if try.MethodDetails != `{"card":{"last4":"4242","name":"S CUSTOMER"}}` {
			t.Errorf("method details = %s", try.MethodDetails)
		}
		if try.RiskDetails != `{"score":62,"cvv_check":"match"}` {
			t.Errorf("risk details = %s", try.RiskDetails)
		}
	})
}

func TestFailedTrySurvivesLaterCapture(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		// The first failure leaves the order open with another attempt's chance, then a capture.
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending, ProviderPaymentID: "pp_0", FailureCode: "insufficient_funds", FailureMessage: "No funds."})
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR", ProviderPaymentID: "pp_1", RawStatus: "captured"})
		got := triesOf(t, e, p.ID)
		if len(got) != 2 {
			t.Fatalf("tries = %+v", got)
		}
		byID := map[string]store.ProviderPaymentAttempt{got[0].ProviderPaymentID: got[0], got[1].ProviderPaymentID: got[1]}
		if f := byID["pp_0"]; f.Status != "failed" || f.FailureMessage != "No funds." {
			t.Errorf("failed try = %+v", f)
		}
		if c := byID["pp_1"]; c.Status != "captured" || c.FailureCode != "" {
			t.Errorf("captured try = %+v", c)
		}
		att, err := e.svc.GetAttempt(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if att.FailureCode != "" || att.FailureMessage != "" {
			t.Errorf("the attempt's current state should hold no failure: %+v", att)
		}
	})
}

func TestLateFailureAfterCapture(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR", ProviderPaymentID: "pp_1"})
		e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pp_0", FailureCode: "declined", FailureMessage: "Declined."})
		if got := e.get(p.ID); got.Status != "succeeded" {
			t.Fatalf("payment %s", got.Status)
		}
		found := false
		for _, try := range triesOf(t, e, p.ID) {
			found = found || (try.ProviderPaymentID == "pp_0" && try.FailureCode == "declined")
		}
		if !found {
			t.Fatal("the late failed try was not recorded")
		}
	})
}

func TestOpenFailureStoredAsFailed(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending, ProviderPaymentID: "pi_1", TryID: "ch_1", RawStatus: "requires_payment_method", FailureCode: "card_declined"})
		got := triesOf(t, e, p.ID)
		if len(got) != 1 || got[0].Status != "failed" || got[0].TryID != "ch_1" {
			t.Fatalf("tries = %+v", got)
		}
	})
}

// Analytics counts status = 'captured': a paid try must not depend on the
// provider's spelling of it.
func TestPaidTryStoredAsCaptured(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, ProviderPaymentID: "pi_1", TryID: "ch_1", RawStatus: "complete/paid", Amount: 50000, Currency: "INR"})
		got := triesOf(t, e, p.ID)
		if len(got) != 1 || got[0].Status != "captured" {
			t.Fatalf("tries = %+v", got)
		}
	})
}

func upsertTry(t *testing.T, e *env, try store.ProviderPaymentAttempt) {
	t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.UpsertProviderPaymentAttempt(ctx, tx, try) }); err != nil {
		t.Fatal(err)
	}
}

// A customer's next go can report the failed charge as the latest one, still open.
func TestFinishedTryDoesNotGoBackToOpen(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		for _, finished := range []string{"failed", "captured"} {
			try := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", TryID: "ch_" + finished, AttemptID: a.ID, Status: finished}
			upsertTry(t, e, try)
			try.Status = "open/unpaid"
			upsertTry(t, e, try)
		}
		for _, got := range triesOf(t, e, p.ID) {
			if got.Status == "open/unpaid" {
				t.Fatalf("tries = %+v", got)
			}
		}
	})
}

// A row keyed by the provider payment stands in for tries not yet told apart:
// from before tries were kept, or from a sync that has only the attempt.
func TestKeyedTryReplacesStandIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		base := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", AttemptID: a.ID, Status: "captured"}
		upsertTry(t, e, base)
		keyed := base
		keyed.TryID = "ch_1"
		upsertTry(t, e, keyed)
		got := triesOf(t, e, p.ID)
		if len(got) != 1 || got[0].TryID != "ch_1" {
			t.Fatalf("tries = %+v", got)
		}
	})
}

// A recorded failure is never erased, even by a later success on the same intent.
func TestFailedStandInSurvivesKeyedSuccess(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		base := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", AttemptID: a.ID, Status: "failed", FailureCode: "card_declined"}
		upsertTry(t, e, base)
		keyed := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", TryID: "ch_2", AttemptID: a.ID, Status: "captured"}
		upsertTry(t, e, keyed)
		if got := triesOf(t, e, p.ID); len(got) != 2 {
			t.Fatalf("tries = %+v", got)
		}
	})
}

func TestTryStoresSnapshotMeaningOverRawWording(t *testing.T) {
	for _, c := range []struct {
		status state.AttemptStatus
		raw    string
	}{{state.AttemptFailed, "failure"}, {state.AttemptAuthorized, "requires_capture"}} {
		storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
			e := newEnv(t, db)
			p := e.payment(50000)
			a := e.attempt(p.ID)
			e.apply(a.ID, core.Snapshot{Status: c.status, ProviderPaymentID: "pi_1", RawStatus: c.raw, Amount: 50000, Currency: "INR"})
			got := triesOf(t, e, p.ID)
			if len(got) != 1 || got[0].Status != string(c.status) {
				t.Errorf("%s: tries = %+v", c.raw, got)
			}
		})
	}
}

// A snapshot that turns out to be another attempt's must not take its try away.
func TestTryStaysWithItsAttempt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		other := e.attempt(p.ID)
		try := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", AttemptID: a.ID, Status: "captured"}
		upsertTry(t, e, try)
		try.AttemptID = other.ID
		upsertTry(t, e, try)
		if got := triesOf(t, e, p.ID); len(got) != 1 || got[0].AttemptID != a.ID {
			t.Fatalf("tries = %+v, want attempt %s", got, a.ID)
		}
	})
}

func TestTryWriteFailureDoesNotBlockApply(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `ALTER TABLE provider_payment_attempts RENAME TO provider_payment_attempts_gone`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r := e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pp_1", FailureCode: "declined"})
		if r.Attempt.Status != "failed" || r.Attempt.FailureCode != "declined" {
			t.Fatalf("attempt = %+v", r.Attempt)
		}
	})
}

func TestRecordTryKeepsNameInBounds(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pp_1",
			MethodDetails: connector.PaymentMethodDetails{Card: &connector.CardDetails{Name: "  "}}})
		if got := triesOf(t, e, p.ID); len(got) != 1 || got[0].MethodDetails != "" {
			t.Fatalf("a blank name is no detail: %+v", got)
		}
	})
}

// A stand-in kept for its failure can't also stand for the capture that followed.
func TestCapturedStandInWithFailureIsNotPaidTwice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		upsertTry(t, e, store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", AttemptID: a.ID, Status: "captured", FailureCode: "card_declined"})
		upsertTry(t, e, store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", TryID: "ch_2", AttemptID: a.ID, Status: "captured"})
		got := triesOf(t, e, p.ID)
		if len(got) != 2 {
			t.Fatalf("tries = %+v", got)
		}
		for _, try := range got {
			want := "failed"
			if try.TryID == "ch_2" {
				want = "captured"
			}
			if try.Status != want {
				t.Errorf("try %s status = %s, want %s", try.TryID, try.Status, want)
			}
		}
	})
}

// A keyed try that isn't a capture says nothing about whether the payment was
// paid, so the row recording that it was stays.
func TestCapturedStandInSurvivesOtherKeyedTries(t *testing.T) {
	for _, keyed := range []store.ProviderPaymentAttempt{
		{TryID: "ch_1", Status: "failed", FailureCode: "card_declined"},
		{TryID: "ch_1", Status: "open/unpaid"},
	} {
		storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
			e := newEnv(t, db)
			p := e.payment(50000)
			a := e.attempt(p.ID)
			upsertTry(t, e, store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", AttemptID: a.ID, Status: "captured"})
			keyed.ProviderAccountID, keyed.ProviderPaymentID, keyed.AttemptID = a.ProviderAccountID, "pi_1", a.ID
			upsertTry(t, e, keyed)
			got := triesOf(t, e, p.ID)
			if len(got) != 2 {
				t.Fatalf("%s: tries = %+v", keyed.Status, got)
			}
			for _, try := range got {
				if try.TryID == "pi_1" && try.Status != "captured" {
					t.Errorf("%s: stand-in = %+v", keyed.Status, try)
				}
			}
		})
	}
}

// With no provider time a try is dated when it was first seen, which is never
// before it happened. So the earliest time known for a try is the best one.
func TestTryKeepsItsEarliestKnownTime(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		polled := time.UnixMilli(1760000900000)
		try := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: "pi_1", TryID: "pi_1:pm_1:card_declined", AttemptID: a.ID, Status: "failed", CreatedAt: polled}
		upsertTry(t, e, try)
		for _, step := range []struct {
			at, want time.Time
		}{
			{time.UnixMilli(1760000100000), time.UnixMilli(1760000100000)},
			{time.UnixMilli(1760000500000), time.UnixMilli(1760000100000)},
		} {
			try.CreatedAt = step.at
			upsertTry(t, e, try)
			if got := triesOf(t, e, p.ID); len(got) != 1 || !got[0].CreatedAt.Equal(step.want) {
				t.Fatalf("after %v: tries = %+v", step.at, got)
			}
		}
	})
}
