package seams

import (
	"strings"
	"testing"
)

const fixMod = "example.com/fix"

func check(t *testing.T, dir string) []Violation {
	t.Helper()
	v, err := Check("testdata/"+dir, fixMod)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantOne(t *testing.T, v []Violation, rule string) Violation {
	t.Helper()
	if len(v) != 1 || v[0].Rule != rule {
		t.Fatalf("want one %s violation, got %+v", rule, v)
	}
	return v[0]
}

func TestCleanFixture(t *testing.T) {
	if v := check(t, "clean"); len(v) != 0 {
		t.Fatalf("unexpected: %+v", v)
	}
}

func TestSQLLeak(t *testing.T) { wantOne(t, check(t, "sqlleak"), "sql-outside-store") }

func TestPgxLeak(t *testing.T) { wantOne(t, check(t, "pgxleak"), "sql-outside-store") }

func TestConnectorLeak(t *testing.T) {
	v := wantOne(t, check(t, "connleak"), "connector-imports")
	if !strings.Contains(v.Detail, "example.com/fix/internal/store") {
		t.Fatalf("detail does not name the import: %q", v.Detail)
	}
}

func TestFatMain(t *testing.T) { wantOne(t, check(t, "fatmain"), "thin-main") }

func TestRepoHasNoViolations(t *testing.T) {
	v, err := Check("../..", "github.com/paymux/paymux")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("violations: %+v", v)
	}
}
