package config

import (
	"strings"
	"testing"
)

// Audit H-9: a PostgreSQL connection that leaves the machine without a verified
// certificate is reported at startup; verify-full is what the docs recommend.
func TestDatabaseTLSWarning(t *testing.T) {
	noenv := func(string) string { return "" }
	cases := []struct {
		url  string
		warn bool
	}{
		{"postgres://u:p@db.internal:5432/paymux", true},
		{"postgres://u:p@db.internal:5432/paymux?sslmode=disable", true},
		{"postgres://u:p@db.internal:5432/paymux?sslmode=allow", true},
		{"postgres://u:p@db.internal:5432/paymux?sslmode=prefer", true},
		{"postgres://u:p@db.internal:5432/paymux?sslmode=require", true},
		{"postgres://u:p@db.internal/paymux?sslmode=REQUIRE", true},
		{"postgres://u:p@10.0.0.5:5432/paymux?sslmode=require", true},
		{"postgres://u:p@db.internal:5432/paymux?sslmode=verify-full", false},
		{"postgres://u:p@db.internal:5432/paymux?sslmode=verify-ca", false},
		{"postgres://u:p@localhost:5432/paymux?sslmode=disable", false},
		{"postgres://u:p@127.0.0.1:5432/paymux", false},
		{"postgres://u:p@[::1]:5432/paymux", false},
		{"postgres://u:p@/paymux?host=/var/run/postgresql", false},
		{"sqlite:///data/paymux.db", false},
		{"", false},
	}
	for _, c := range cases {
		got := DatabaseTLSWarning(c.url, noenv)
		if (got != "") != c.warn {
			t.Errorf("%s: warning %q, want warning=%v", c.url, got, c.warn)
		}
		if got != "" && (!strings.Contains(got, "verify-full") || strings.Contains(got, "u:p") || strings.Contains(got, ":p@")) {
			t.Errorf("%s: the message must recommend verify-full and carry no credentials: %q", c.url, got)
		}
	}
	// PGSSLMODE is the driver's default for a URL without sslmode.
	env := func(k string) string {
		if k == "PGSSLMODE" {
			return "verify-full"
		}
		return ""
	}
	if got := DatabaseTLSWarning("postgres://u:p@db.internal/paymux", env); got != "" {
		t.Errorf("PGSSLMODE=verify-full: %q", got)
	}
	if got := DatabaseTLSWarning("postgres://u:p@db.internal/paymux?sslmode=require", env); got == "" {
		t.Error("an explicit sslmode wins over PGSSLMODE")
	}
}
