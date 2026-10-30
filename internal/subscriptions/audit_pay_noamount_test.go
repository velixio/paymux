package subscriptions

import (
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// Audit 2026-10 (A-2): a success that states no amount or currency was given
// the expected charge, so the core's "captured needs an amount" and amount
// mismatch checks never saw the gap and the charge was booked as paid in full.
func TestAuditPaySuccessWithoutAmountIsNotTheExpectedCharge(t *testing.T) {
	snap := toSnapshot(connector.PaymentSnapshot{Status: connector.PaymentSucceeded, ProviderPaymentID: "pay_1"}, 49900, "INR")
	if snap.Amount != 0 || snap.Currency != "" {
		t.Fatalf("a success with no amount was booked as %d %s", snap.Amount, snap.Currency)
	}
	snap = toSnapshot(connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 100, Currency: "USD"}, 49900, "INR")
	if snap.Amount != 100 || snap.Currency != "USD" {
		t.Fatalf("the provider's amount was replaced: %d %s", snap.Amount, snap.Currency)
	}
}
