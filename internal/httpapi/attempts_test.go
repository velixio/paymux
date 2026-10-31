package httpapi_test

import (
	"testing"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
)

func TestGetPaymentListsItsAttempts(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		_, m := f.createPayment(createBody)
		id := m["id"].(string)

		r := f.do("GET", "/v1/payments/"+id, f.testKey, nil)
		read := r.json(t)
		if read["checkout_mode"] != "hosted" {
			t.Fatalf("GET checkout_mode = %v, want hosted: %s", read["checkout_mode"], r.raw)
		}
		first := read["attempts"]
		if list, ok := first.([]any); !ok || len(list) != 0 {
			t.Fatalf("a payment with no attempts must show attempts: [] , got %v (%s)", first, r.raw)
		}

		a1, err := f.svc.CreateAttempt(ctx, id, f.pa)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.ApplyAttemptSnapshot(ctx, a1.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pay_bad", Method: "card",
			FailureCode: "card_declined", FailureMessage: "Your card was declined."}); err != nil {
			t.Fatal(err)
		}
		a2, err := f.svc.CreateAttempt(ctx, id, f.pa)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.ApplyAttemptSnapshot(ctx, a2.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 49900, Currency: "INR",
			ProviderPaymentID: "pay_good", Method: "upi"}); err != nil {
			t.Fatal(err)
		}

		got := f.do("GET", "/v1/payments/"+id, f.testKey, nil).json(t)
		list, _ := got["attempts"].([]any)
		if len(list) != 2 {
			t.Fatalf("attempts: %v", got["attempts"])
		}
		// Attempts created in the same millisecond have no defined order: pick them by id.
		x, y := list[0].(map[string]any), list[1].(map[string]any)
		if x["id"] != a1.ID {
			x, y = y, x
		}
		if x["id"] != a1.ID || x["status"] != "failed" || x["provider"] != "fake" || x["provider_payment_id"] != "pay_bad" || x["method"] != "card" ||
			x["failure_code"] != "card_declined" || x["failure_message"] != "Your card was declined." || x["created_at"] == nil {
			t.Errorf("first attempt %v", x)
		}
		if y["id"] != a2.ID || y["status"] != "captured" || y["provider_payment_id"] != "pay_good" || y["method"] != "upi" {
			t.Errorf("second attempt %v", y)
		}
		if _, has := y["failure_code"]; has {
			t.Errorf("a successful attempt should not carry a failure code: %v", y)
		}
		// Nothing internal leaks: no account ids, secrets or raw provider payloads.
		for _, k := range []string{"provider_account_id", "provider_ref", "raw_last", "next_action", "provider_order_id"} {
			if _, has := x[k]; has {
				t.Errorf("attempt exposes %s", k)
			}
		}

		// List and create responses stay light: attempts only on the single-payment read.
		if lr := f.do("GET", "/v1/payments", f.testKey, nil).json(t); lr["data"].([]any)[0].(map[string]any)["attempts"] != nil {
			t.Error("the list view should not carry attempts")
		}
		// Another app cannot see them.
		if r := f.do("GET", "/v1/payments/"+id, f.otherKey, nil); r.status != 404 {
			t.Errorf("other app: %d", r.status)
		}
	})
}
