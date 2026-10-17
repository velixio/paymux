package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paymux/paymux/migrations"
)

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	lookup := func(string) (string, bool) { return "", false }
	code := Run(context.Background(), args, lookup, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunVersion(t *testing.T) {
	code, out, _ := run("version")
	if code != 0 || out != "paymux dev\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	code, _, errs := run("bogus")
	if code != 2 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{`unknown command "bogus"`, "usage: paymux <serve|migrate|backup|db|app|provider|domain|tax-entity|report|fx|endpoint|version>"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr %q missing %q", errs, want)
		}
	}
}

func TestRunNoArgs(t *testing.T) {
	code, _, errs := run()
	if code != 2 || !strings.Contains(errs, "usage: paymux <serve|migrate|backup|db|app|provider|domain|tax-entity|report|fx|endpoint|version>") {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestServeReportsMalformedDotEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(p, []byte("A=1\nNOEQUALS\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	lookup := func(k string) (string, bool) {
		if k == "PAYMUX_ENV_FILE" {
			return p, true
		}
		return "", false
	}
	code := Run(context.Background(), []string{"serve"}, lookup, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "line 2") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func migrateEnv(dir string) func(string) (string, bool) {
	env := map[string]string{
		"PAYMUX_ENV":          "development",
		"PAYMUX_BASE_URL":     "http://localhost:8080",
		"PAYMUX_DATA_DIR":     dir,
		"PAYMUX_ENV_FILE":     filepath.Join(dir, "none.env"),
		"PAYMUX_MASTER_KEY":   "YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE=",
		"PAYMUX_DEV_SKIP_2FA": "true",
	}
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestMigrateAppliesThenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	lookup := migrateEnv(dir)
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"migrate"}, lookup, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if entries, _ := migrations.FS.ReadDir("sqlite"); !strings.Contains(out.String(), fmt.Sprintf("applied %d migrations (sqlite)", len(entries))) {
		t.Fatalf("stdout=%q", out.String())
	}
	out.Reset()
	if code := Run(context.Background(), []string{"migrate"}, lookup, &out, &errb); code != 0 {
		t.Fatalf("second run code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "applied 0 migrations (sqlite)") {
		t.Fatalf("stdout=%q", out.String())
	}
}

func TestServeMissingConfigExit1(t *testing.T) {
	var out, errb bytes.Buffer
	lookup := func(k string) (string, bool) {
		if k == "PAYMUX_ENV_FILE" {
			return filepath.Join(t.TempDir(), "absent.env"), true
		}
		return "", false
	}
	code := Run(context.Background(), []string{"serve"}, lookup, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "PAYMUX_BASE_URL is required") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}
