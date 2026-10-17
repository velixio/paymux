package settings

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The UI draws its controls from Meta. These tests pin every Meta to the
// validator of its setting, so a control can never offer a value the server
// would refuse, nor hide one it accepts.

func ok(t *testing.T, d Def, raw string, want bool) {
	t.Helper()
	err := d.Validate(json.RawMessage(raw))
	if want && err != nil {
		t.Errorf("%s: %s should be valid: %v", d.Key, raw, err)
	}
	if !want && err == nil {
		t.Errorf("%s: %s should be invalid", d.Key, raw)
	}
}

func TestEverySettingHasMeta(t *testing.T) {
	for _, d := range Defs() {
		m, found := MetaFor(d.Key)
		if !found {
			t.Errorf("%s has no Meta", d.Key)
			continue
		}
		if strings.TrimSpace(m.Help) == "" {
			t.Errorf("%s has no help text", d.Key)
		}
		if m.Kind == "" {
			t.Errorf("%s has no kind", d.Key)
		}
		ok(t, d, d.Default, true) // the built-in default is itself valid
	}
	if _, found := MetaFor("nope"); found {
		t.Error("Meta for an unknown key")
	}
}

func TestMetaAgreesWithValidators(t *testing.T) {
	for _, d := range Defs() {
		m, _ := MetaFor(d.Key)
		switch m.Kind {
		case KindEnum:
			if len(m.Options) < 2 {
				t.Errorf("%s: enum with %d options", d.Key, len(m.Options))
			}
			for _, o := range m.Options {
				ok(t, d, fmt.Sprintf("%q", o), true)
			}
			ok(t, d, `"bogus"`, false)
		case KindBool:
			ok(t, d, "true", true)
			ok(t, d, "false", true)
			ok(t, d, `"yes"`, false)
		case KindInt:
			lo, hi := int64(m.Min), int64(m.Max)
			ok(t, d, fmt.Sprint(lo), true)
			if m.Max != 0 {
				ok(t, d, fmt.Sprint(hi), true)
				ok(t, d, fmt.Sprint(hi+1), false)
			}
			if m.ZeroOK {
				ok(t, d, "0", true)
			}
			if lo > 0 && !m.ZeroOK || lo > 1 && m.ZeroOK {
				ok(t, d, fmt.Sprint(lo-1), false)
			}
			ok(t, d, `"x"`, false)
		case KindFloat:
			ok(t, d, fmt.Sprint(m.Min), true)
			ok(t, d, fmt.Sprint(m.Max), true)
			ok(t, d, fmt.Sprint(m.Min-0.5), false)
			ok(t, d, fmt.Sprint(m.Max+0.5), false)
		case KindString:
			ok(t, d, "1", false) // not a string
		case KindClock:
			ok(t, d, `"00:00"`, true)
			ok(t, d, `"23:59"`, true)
			ok(t, d, `"24:00"`, false)
		case KindList:
			item := func(v int64) string { return fmt.Sprint(v) }
			mk := func(n int, v int64) string {
				parts := make([]string, n)
				for i := range parts {
					parts[i] = item(v)
				}
				return "[" + strings.Join(parts, ",") + "]"
			}
			lo := int64(m.Min)
			ok(t, d, mk(m.MinItems, lo), true)
			ok(t, d, mk(m.MaxItems, lo), true)
			ok(t, d, mk(m.MaxItems+1, lo), false)
			if m.MinItems > 0 {
				ok(t, d, mk(m.MinItems-1, lo), false)
			}
			if m.Max != 0 {
				ok(t, d, mk(1, int64(m.Max)), true)
				ok(t, d, mk(1, int64(m.Max)+1), false)
			}
			if lo > 0 {
				ok(t, d, mk(1, lo-1), false)
			} else {
				ok(t, d, mk(1, -1), false)
			}
		case KindJSON:
			// free-form documents: nothing to pin
		default:
			t.Errorf("%s: unknown kind %q", d.Key, m.Kind)
		}
		if m.Kind == KindString && m.Pattern == "" && d.Key != "" {
			t.Errorf("%s: a string setting needs a pattern", d.Key)
		}
	}
}

func TestStringPatternsMatchTheirValidators(t *testing.T) {
	cases := map[string][2]string{ // key -> {valid example, invalid example}
		"sac_code":                  {"998315", "12345"},
		"invoice_series_prefix":     {"INV", "toolong1"},
		"credit_note_series_prefix": {"CN", "lower"},
	}
	for key, c := range cases {
		d, _ := Lookup(key)
		m, _ := MetaFor(key)
		if m.Pattern == "" {
			t.Fatalf("%s: no pattern", key)
		}
		ok(t, d, fmt.Sprintf("%q", c[0]), true)
		ok(t, d, fmt.Sprintf("%q", c[1]), false)
	}
}
