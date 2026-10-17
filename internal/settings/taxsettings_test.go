package settings_test

import (
	"encoding/json"
	"testing"

	"github.com/paymux/paymux/internal/settings"
)

func TestTaxExceptionSettingsValidate(t *testing.T) {
	cases := []struct {
		key, val string
		ok       bool
	}{
		{"credit_note_cutoff", `"11-30"`, true},
		{"credit_note_cutoff", `"12-31"`, true},
		{"credit_note_cutoff", `"off"`, true},
		{"credit_note_cutoff", `"02-29"`, true},
		{"credit_note_cutoff", `"13-01"`, false},
		{"credit_note_cutoff", `"02-30"`, false},
		{"credit_note_cutoff", `"1-5"`, false},
		{"credit_note_cutoff", `30`, false},
		{"b2cl_threshold", `10000000`, true},
		{"b2cl_threshold", `1`, true},
		{"b2cl_threshold", `0`, false},
		{"b2cl_threshold", `-5`, false},
		{"b2cl_threshold", `"100"`, false},
	}
	for _, c := range cases {
		d, found := settings.Lookup(c.key)
		if !found {
			t.Fatalf("no setting %s", c.key)
		}
		err := d.Validate(json.RawMessage(c.val))
		if (err == nil) != c.ok {
			t.Errorf("%s=%s: err=%v, want ok=%v", c.key, c.val, err, c.ok)
		}
	}
	// The built-in values are today's rules.
	if d, _ := settings.Lookup("credit_note_cutoff"); d.Default != `"11-30"` || d.Levels != settings.LevelOrg {
		t.Errorf("credit_note_cutoff default/levels = %s %v", d.Default, d.Levels)
	}
	if d, _ := settings.Lookup("b2cl_threshold"); d.Default != `10000000` || d.Levels != settings.LevelOrg {
		t.Errorf("b2cl_threshold default/levels = %s %v", d.Default, d.Levels)
	}
}
