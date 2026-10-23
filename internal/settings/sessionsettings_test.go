package settings_test

import (
	"encoding/json"
	"testing"

	"github.com/paymux/paymux/internal/settings"
)

func TestSessionLifetimeSettings(t *testing.T) {
	for _, c := range []struct {
		key, val string
		ok       bool
	}{
		{"session_idle_hours", `12`, true}, {"session_idle_hours", `1`, true}, {"session_idle_hours", `720`, true},
		{"session_idle_hours", `0`, false}, {"session_idle_hours", `721`, false}, {"session_idle_hours", `"12"`, false},
		{"session_max_days", `7`, true}, {"session_max_days", `1`, true}, {"session_max_days", `90`, true},
		{"session_max_days", `0`, false}, {"session_max_days", `91`, false},
		{"session_remember_days", `30`, true}, {"session_remember_days", `1`, true}, {"session_remember_days", `90`, true},
		{"session_remember_days", `0`, true}, // switches "remember me" off
		{"session_remember_days", `91`, false}, {"session_remember_days", `-1`, false}, {"session_remember_days", `"30"`, false},
	} {
		d, ok := settings.Lookup(c.key)
		if !ok {
			t.Fatalf("no setting %s", c.key)
		}
		if err := d.Validate(json.RawMessage(c.val)); (err == nil) != c.ok {
			t.Errorf("%s=%s: err=%v, want ok=%v", c.key, c.val, err, c.ok)
		}
	}
	for key, want := range map[string]string{"session_idle_hours": "12", "session_max_days": "7", "session_remember_days": "30"} {
		d, _ := settings.Lookup(key)
		if d.Default != want || d.Levels != settings.LevelOrg {
			t.Errorf("%s default/levels = %s %v", key, d.Default, d.Levels)
		}
		if _, ok := settings.MetaFor(key); !ok {
			t.Errorf("%s has no description for the settings page", key)
		}
	}
}
