package core_test

import (
	"testing"

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
