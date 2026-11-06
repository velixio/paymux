package core_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

func TestPaymentAttemptsIncludeTries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		t0 := time.UnixMilli(1760000000000)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending, ProviderPaymentID: "pi_1", TryID: "ch_2", RawStatus: "requires_action",
			ProviderCreatedAt: t0.Add(time.Minute), FailureMessage: "Second."})
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending, ProviderPaymentID: "pi_1", TryID: "ch_1", RawStatus: "requires_payment_method",
			ProviderCreatedAt: t0, FailureCode: "insufficient_funds", FailureMessage: "First.", ThreeDS: connector.ThreeDSPassed,
			MethodDetails: connector.PaymentMethodDetails{Card: &connector.CardDetails{Last4: "4242", Name: "S CUSTOMER"}}})
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending, ProviderPaymentID: "pi_1", TryID: "ch_3", RawStatus: "succeeded", ProviderCreatedAt: t0.Add(2 * time.Minute)})
		views, err := e.svc.PaymentAttempts(ctx, e.scope, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || len(views[0].Tries) != 3 {
			t.Fatalf("views = %+v", views)
		}
		tries := views[0].Tries
		if tries[0].FailureMessage != "First." || tries[1].FailureMessage != "Second." {
			t.Fatalf("tries out of order: %+v", tries)
		}
		if tries[0].Status != "failed" || tries[2].Status != "captured" {
			t.Errorf("statuses: %s, %s", tries[0].Status, tries[2].Status)
		}
		if tries[0].ThreeDSecure == nil || tries[0].ThreeDSecure.Status != "passed" {
			t.Errorf("3D Secure: %+v", tries[0].ThreeDSecure)
		}
		if tries[0].MethodDetails == nil || tries[0].MethodDetails.Card.Name != "S CUSTOMER" {
			t.Errorf("method details: %+v", tries[0].MethodDetails)
		}
	})
}

func TestTryJSONLeavesOutMissingFields(t *testing.T) {
	raw, err := json.Marshal(core.TryView{Status: "pending", CreatedAt: time.UnixMilli(1760000000000)})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"three_d_secure", "method_details", "failure_code", "failure_message", "method", "provider_payment_id"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("%s in %s", key, raw)
		}
	}
}

func TestAttemptWithoutTriesHasEmptyList(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		e.attempt(p.ID)
		views, err := e.svc.PaymentAttempts(ctx, e.scope, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(views)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"tries":[]`) {
			t.Fatalf("no empty list: %s", raw)
		}
	})
}

func TestCheckoutErrorKeepsAttemptFailure(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, FailureCode: "checkout_error", FailureMessage: "provider down"})
		views, err := e.svc.PaymentAttempts(ctx, e.scope, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || views[0].FailureCode != "checkout_error" || views[0].FailureMessage != "provider down" || len(views[0].Tries) != 0 {
			t.Fatalf("views = %+v", views)
		}
	})
}

func eventJSON(t *testing.T, e *env, typ string) map[string]any {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].Type == typ {
			raw, err := json.Marshal(e.events[i].Data)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("no %s event", typ)
	return nil
}

func TestPaymentFailedEventCarriesFailure(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pp_1", FailureCode: "insufficient_funds", FailureMessage: "No funds."})
		data := eventJSON(t, e, "payment.failed")
		if data["failure_code"] != "insufficient_funds" || data["failure_message"] != "No funds." || data["attempt_id"] != a.ID {
			t.Fatalf("data = %v", data)
		}
	})
}

func TestOtherEventsLeaveFailureOut(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p := e.payment(50000)
		a := e.attempt(p.ID)
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR", ProviderPaymentID: "pp_1", FailureCode: "stale", FailureMessage: "Stale."})
		data := eventJSON(t, e, "payment.succeeded")
		if _, ok := data["failure_code"]; ok {
			t.Fatalf("data = %v", data)
		}
		if _, ok := data["failure_message"]; ok {
			t.Fatalf("data = %v", data)
		}
	})
}
