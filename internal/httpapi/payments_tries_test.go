package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/pkg/connector"
)

func triedPayment(t *testing.T, f *fixture) string {
	t.Helper()
	_, m := f.createPayment(createBody)
	id := m["id"].(string)
	a, err := f.svc.CreateAttempt(ctx, id, f.pa)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptFailed, ProviderPaymentID: "pay_bad", Method: "card",
		FailureCode: "card_declined", FailureMessage: "Your card was declined.", ThreeDS: connector.ThreeDSFailed}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGetPaymentShowsTries(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		id := triedPayment(t, f)
		got := f.do("GET", "/v1/payments/"+id, f.testKey, nil).json(t)
		attempts := got["attempts"].([]any)
		tries, _ := attempts[0].(map[string]any)["tries"].([]any)
		if len(tries) != 1 {
			t.Fatalf("tries = %v", attempts[0])
		}
		try := tries[0].(map[string]any)
		if try["status"] != "failed" || try["failure_code"] != "card_declined" || try["failure_message"] != "Your card was declined." || try["provider_payment_id"] != "pay_bad" {
			t.Errorf("try = %v", try)
		}
		if tds, _ := try["three_d_secure"].(map[string]any); tds["status"] != "failed" {
			t.Errorf("3D Secure = %v", try["three_d_secure"])
		}
	})
}

func TestTriesScopedToAppAndMode(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		id := triedPayment(t, f)
		for name, key := range map[string]string{"other mode": f.liveKey, "other app": f.otherKey} {
			if r := f.do("GET", "/v1/payments/"+id, key, nil); r.status != http.StatusNotFound {
				t.Errorf("%s: status %d: %s", name, r.status, r.raw)
			}
		}
	})
}
