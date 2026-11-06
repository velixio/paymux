package settings

import (
	"encoding/json"
	"testing"
)

func TestTerminalLogSettings(t *testing.T) {
	for _, c := range []struct {
		key, val string
		ok       bool
	}{
		{"terminal_log_level", `"error"`, true},
		{"terminal_log_level", `"warn"`, true},
		{"terminal_log_level", `"info"`, true},
		{"terminal_log_level", `"debug"`, true},
		{"terminal_log_level", `"trace"`, false},
		{"terminal_log_level", `"INFO"`, false},
		{"terminal_log_level", `1`, false},
		{"terminal_log_requests", `true`, true},
		{"terminal_log_requests", `false`, true},
		{"terminal_log_requests", `"on"`, false},
	} {
		d, ok := Lookup(c.key)
		if !ok {
			t.Fatalf("no setting %s", c.key)
		}
		if err := d.Validate(json.RawMessage(c.val)); (err == nil) != c.ok {
			t.Errorf("%s=%s: err=%v, want ok=%v", c.key, c.val, err, c.ok)
		}
	}
}

// The defaults keep the terminal printing what it printed before the settings.
func TestTerminalLogDefaults(t *testing.T) {
	for key, want := range map[string]string{"terminal_log_level": `"info"`, "terminal_log_requests": `true`} {
		d, ok := Lookup(key)
		if !ok {
			t.Fatalf("no setting %s", key)
		}
		if d.Default != want {
			t.Errorf("%s default = %s, want %s", key, d.Default, want)
		}
		if d.Levels != LevelOrg {
			t.Errorf("%s must be set at the org level only", key)
		}
		if !AdminOnly(key) {
			t.Errorf("%s must be admin only", key)
		}
	}
}
