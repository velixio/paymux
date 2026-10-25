package settings

import "testing"

// The sign-in policy and the e-invoice trust anchor are admin settings: a
// custom role holding settings.write must not reach them (audit H-3).
func TestAdminOnlyKeys(t *testing.T) {
	for _, k := range []string{"session_idle_hours", "session_max_days", "session_remember_days", "einvoice_irp_keys"} {
		if !AdminOnly(k) {
			t.Errorf("%s must be admin only", k)
		}
	}
	for _, k := range []string{"payment_ttl", "theme", "audit_retention_days"} {
		if AdminOnly(k) {
			t.Errorf("%s must not be admin only", k)
		}
	}
}
