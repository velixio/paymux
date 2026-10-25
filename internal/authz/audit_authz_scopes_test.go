package authz

import (
	"slices"
	"testing"
)

// Audit (2026-10, authz lane). F29: "a key is a delegation, so it may not carry
// power its issuer lacks". payments:write and subscriptions:write also cover
// POST /v1/payments/{id}/cancel and POST /v1/subscriptions/{id}/cancel, which the
// API authorises as PaymentCancel, but ActionsForScopes only asks the issuer for
// PaymentCreate. A custom role {api_key.create, payment.create, refund.create}
// (no payment.cancel) can therefore mint a key that cancels payments and
// subscriptions. This test fails on the current code.
func TestAuditAuthzKeyScopesNeedCancelPower(t *testing.T) {
	for _, scopes := range [][]string{nil, {ScopePaymentsWrite}, {ScopeSubscriptionsWrite}} {
		need := ActionsForScopes(scopes)
		if !slices.Contains(need, PaymentCancel) {
			t.Errorf("ActionsForScopes(%v) = %v: a key with these scopes can cancel (PaymentCancel), the issuer is not asked for it", scopes, need)
		}
	}
}
