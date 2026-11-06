package state

import "testing"

// Stripe's session snapshots report "<session status>/<payment status>".
func TestTryStatusStripeSession(t *testing.T) {
	for raw, want := range map[string]AttemptStatus{
		"complete/paid": AttemptCaptured, "complete/no_payment_required": AttemptCaptured,
		"expired/unpaid": AttemptFailed, "open/unpaid": AttemptPending, "complete/unpaid": AttemptPending,
	} {
		if got := TryStatus(raw); got != want {
			t.Errorf("TryStatus(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestTryStatus(t *testing.T) {
	for raw, want := range map[string]AttemptStatus{
		"captured": AttemptCaptured, "SUCCEEDED": AttemptCaptured, "paid": AttemptCaptured,
		"failed": AttemptFailed, "user_dropped": AttemptFailed, "expired": AttemptFailed,
		"authorized": AttemptAuthorized, "processing": AttemptAuthorized,
		"created": AttemptPending, "requires_action": AttemptPending, "anything_new": AttemptPending,
	} {
		if got := TryStatus(raw); got != want {
			t.Errorf("TryStatus(%q) = %s, want %s", raw, got, want)
		}
	}
}
