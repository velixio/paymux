package settings_test

import (
	"encoding/json"
	"testing"

	"github.com/paymux/paymux/internal/settings"
)

func TestLogAndAuditRetentionSettings(t *testing.T) {
	for _, c := range []struct {
		key, val string
		ok       bool
	}{
		{"log_retention_error_days", `90`, true},
		{"log_retention_error_days", `1`, true},
		{"log_retention_error_days", `3650`, true},
		{"log_retention_error_days", `0`, false},
		{"log_retention_error_days", `3651`, false},
		{"log_retention_error_days", `"90"`, false},
		{"log_retention_warn_days", `7`, true},
		{"log_retention_warn_days", `0`, false},
		{"log_retention_warn_days", `366`, false},
		{"log_max_rows", `500000`, true},
		{"log_max_rows", `999`, false},
		{"audit_retention_days", `400`, true},
		{"audit_retention_days", `365`, true},
		{"audit_retention_days", `0`, true}, // forever
		{"audit_retention_days", `364`, false},
		{"audit_retention_days", `30`, false},
		{"audit_retention_days", `-1`, false},
	} {
		d, ok := settings.Lookup(c.key)
		if !ok {
			t.Fatalf("no setting %s", c.key)
		}
		if err := d.Validate(json.RawMessage(c.val)); (err == nil) != c.ok {
			t.Errorf("%s=%s: err=%v, want ok=%v", c.key, c.val, err, c.ok)
		}
	}
	for key, want := range map[string]string{"log_retention_error_days": "90", "log_retention_warn_days": "7", "log_max_rows": "500000", "audit_retention_days": "400"} {
		d, _ := settings.Lookup(key)
		if d.Default != want || d.Levels != settings.LevelOrg {
			t.Errorf("%s default/levels = %s %v", key, d.Default, d.Levels)
		}
	}
}
