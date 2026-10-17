package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, want := range []string{`unknown command "bogus"`, "usage: paymux <serve|migrate|version>"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr %q missing %q", errs, want)
		}
	}
}

func TestRunNoArgs(t *testing.T) {
	code, _, errs := run()
	if code != 2 || !strings.Contains(errs, "usage: paymux <serve|migrate|version>") {
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
