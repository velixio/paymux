package stdwebhooks

import (
	"errors"
	"testing"
)

func TestValidateSecret(t *testing.T) {
	for name, tc := range map[string]struct {
		secret string
		ok     bool
	}{
		"24 bytes":      {"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh", true},
		"64 bytes":      {"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYQ==", true},
		"65 bytes":      {"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE=", false},
		"3 bytes":       {"whsec_YWJj", false},
		"no prefix":     {"YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh", false},
		"not base64":    {"whsec_not!base64!not!base64!not!base64!", false},
		"empty":         {"", false},
		"only a prefix": {"whsec_", false},
	} {
		err := ValidateSecret(tc.secret)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: err = %v, want ErrInvalidSecret", name, err)
		}
	}
}
