package razorpay

import (
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func TestAuthenticationStepFailureIsThreeDSFailed(t *testing.T) {
	snap := paymentEntity{ID: "pay_1", Status: "failed", Method: "card", ErrorStep: "payment_authentication", ErrorReason: "incorrect_otp"}.snapshot()
	if snap.ThreeDS != connector.ThreeDSFailed {
		t.Fatalf("3D Secure = %q", snap.ThreeDS)
	}
}

func TestOtherStepHasNoThreeDS(t *testing.T) {
	for name, p := range map[string]paymentEntity{
		"bank declined":      {ID: "pay_1", Status: "failed", Method: "card", ErrorStep: "payment_authorization"},
		"not a card":         {ID: "pay_1", Status: "failed", Method: "upi", ErrorStep: "payment_authentication"},
		"nothing wrong":      {ID: "pay_1", Status: "captured", Method: "card"},
		"step not available": {ID: "pay_1", Status: "failed", Method: "card", ErrorStep: "NA"},
	} {
		if got := p.snapshot().ThreeDS; got != "" {
			t.Errorf("%s: 3D Secure = %q", name, got)
		}
	}
}
