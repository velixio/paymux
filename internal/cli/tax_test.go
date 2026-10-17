package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

// checksum completes a 14-character GSTIN prefix with its check character.
func checksum(t *testing.T, prefix14 string) byte {
	t.Helper()
	for _, c := range "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		if tax.ValidGSTIN(prefix14+string(c)) == nil {
			return byte(c)
		}
	}
	t.Fatalf("no checksum for %s", prefix14)
	return 0
}

const cliGSTIN = "27AAPFU0939F1ZV"

func TestTaxEntityAndAppTaxCommands(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runIn(dir, "app", "create", "--name", "Voice Studio", "--slug", "vs"); code != 0 {
		t.Fatalf("app create: %d %s", code, errs)
	}
	code, out, errs := runIn(dir, "tax-entity", "add", "--gstin", cliGSTIN, "--legal-name", "Voice Studio Pvt Ltd", "--state", "27",
		"--lut", "AD270226000001A", "--lut-from", "2026-04-01", "--lut-to", "2027-03-31", "--signatory", "A. Signatory")
	if code != 0 || !strings.Contains(out, "created tax entity te_") || !strings.Contains(out, cliGSTIN) {
		t.Fatalf("tax-entity add: %d out=%q err=%q", code, out, errs)
	}
	// The same GSTIN twice is refused.
	if code, _, errs := runIn(dir, "tax-entity", "add", "--gstin", cliGSTIN, "--legal-name", "Again"); code != 2 || !strings.Contains(errs, "already exists") {
		t.Errorf("duplicate: %d %q", code, errs)
	}

	code, out, errs = runIn(dir, "app", "tax", "--app", "vs", "--tax-entity", cliGSTIN, "--sac", "998315", "--rate", "1800", "--inclusive")
	if code != 0 || !strings.Contains(out, "SAC 998315") || !strings.Contains(out, "inclusive=true") {
		t.Fatalf("app tax: %d out=%q err=%q", code, out, errs)
	}

	ctx := context.Background()
	readDB(t, dir, func(q store.Querier) {
		e, err := store.FindTaxEntityByGSTIN(ctx, q, store.DefaultOrgID, cliGSTIN)
		if err != nil || e.LUTNumber != "AD270226000001A" || e.LUTValidTo.IsZero() || e.PAN != "AAPFU0939F" || e.StateCode != "27" ||
			e.InvoicePrefix != "INV" || e.SignatoryName != "A. Signatory" {
			t.Errorf("entity = %+v err=%v", e, err)
		}
		app, _ := store.GetAppBySlug(ctx, q, store.DefaultOrgID, "vs")
		p, err := store.GetAppTaxProfile(ctx, q, app.ID)
		if err != nil || p.TaxEntityID != e.ID || !p.IssueInvoices || p.SACCode != "998315" || p.GSTRateBP != 1800 || !p.PricesIncludeTax {
			t.Errorf("profile = %+v err=%v", p, err)
		}
	})

	// Exclusive pricing and a re-run replaces the profile.
	if code, _, errs := runIn(dir, "app", "tax", "--app", "vs", "--tax-entity", cliGSTIN, "--rate", "500", "--inclusive=false"); code != 0 {
		t.Fatalf("app tax again: %d %s", code, errs)
	}
	readDB(t, dir, func(q store.Querier) {
		app, _ := store.GetAppBySlug(ctx, q, store.DefaultOrgID, "vs")
		if p, _ := store.GetAppTaxProfile(ctx, q, app.ID); p.PricesIncludeTax || p.GSTRateBP != 500 {
			t.Errorf("profile = %+v", p)
		}
	})
}

func TestTaxCommandsRejectBadInput(t *testing.T) {
	dir := t.TempDir()
	runIn(dir, "app", "create", "--name", "A", "--slug", "a")
	runIn(dir, "tax-entity", "add", "--gstin", cliGSTIN, "--legal-name", "X")
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"bad GSTIN", []string{"tax-entity", "add", "--gstin", "27AAPFU0939F1ZW", "--legal-name", "X"}, 2, "GSTIN"},
		{"state mismatch", []string{"tax-entity", "add", "--gstin", "29ABCDE1234F1Z" + string(checksum(t, "29ABCDE1234F1Z")), "--legal-name", "X", "--state", "27"}, 2, "does not match"},
		{"long prefix", []string{"tax-entity", "add", "--gstin", "07ZZZZZ9999Z9Z" + string(checksum(t, "07ZZZZZ9999Z9Z")), "--legal-name", "X", "--invoice-prefix", "TOOLONGX"}, 2, "prefix"},
		{"partial LUT", []string{"tax-entity", "add", "--gstin", "07ZZZZZ9999Z9Z" + string(checksum(t, "07ZZZZZ9999Z9Z")), "--legal-name", "X", "--lut", "N"}, 2, "LUT"},
		{"bad date", []string{"tax-entity", "add", "--gstin", cliGSTIN, "--legal-name", "X", "--lut-from", "17/10/2026"}, 2, "YYYY-MM-DD"},
		{"missing flags", []string{"tax-entity", "add", "--gstin", cliGSTIN}, 2, "usage"},
		{"bad sac", []string{"app", "tax", "--app", "a", "--tax-entity", cliGSTIN, "--sac", "12"}, 2, "SAC"},
		{"bad rate", []string{"app", "tax", "--app", "a", "--tax-entity", cliGSTIN, "--rate", "9999"}, 2, "rate"},
		{"unknown app", []string{"app", "tax", "--app", "nope", "--tax-entity", cliGSTIN}, 2, "no app"},
		{"unknown entity", []string{"app", "tax", "--app", "a", "--tax-entity", "29ZZZZZ0000Z1Z0"}, 2, "no tax entity"},
		{"report bad month", []string{"report", "gst", "--entity", cliGSTIN, "--month", "oct", "--out", t.TempDir()}, 2, "YYYY-MM"},
		{"report usage", []string{"report", "gst"}, 2, "usage"},
		{"fx bad rate", []string{"fx", "set", "--currency", "USD", "--date", "2026-10-17", "--rate", "-1"}, 2, "rate"},
		{"fx inr", []string{"fx", "set", "--currency", "INR", "--date", "2026-10-17", "--rate", "1"}, 2, "foreign"},
	}
	for _, c := range cases {
		code, _, errs := runIn(dir, c.args...)
		if code != c.code || !strings.Contains(errs, c.want) {
			t.Errorf("%s: code=%d stderr=%q, want %d containing %q", c.name, code, errs, c.code, c.want)
		}
	}
}

func TestReportGSTAndFXSetWriteOutput(t *testing.T) {
	dir := t.TempDir()
	runIn(dir, "tax-entity", "add", "--gstin", cliGSTIN, "--legal-name", "X")
	if code, out, errs := runIn(dir, "fx", "set", "--currency", "USD", "--date", "2026-10-17", "--rate", "83.2500"); code != 0 || !strings.Contains(out, "83.2500") {
		t.Fatalf("fx set: %d %q %q", code, out, errs)
	}
	out := filepath.Join(t.TempDir(), "reports")
	code, stdout, errs := runIn(dir, "report", "gst", "--entity", cliGSTIN, "--month", "2026-10", "--out", out)
	if code != 0 {
		t.Fatalf("report: %d %s", code, errs)
	}
	for _, f := range []string{"sales_register.csv", "b2b.csv", "b2cl.csv", "b2cs.csv", "exp.csv", "cdnr.csv", "cdnur.csv", "sac_summary.csv", "documents_issued.csv", "exceptions.csv"} {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil || !strings.Contains(string(b), ",") {
			t.Errorf("%s: %v", f, err)
		}
		if !strings.Contains(stdout, f) {
			t.Errorf("stdout does not list %s", f)
		}
	}
}

// readDB opens the data dir's store just for fn, so the CLI can take the
// SQLite lock between calls.
func readDB(t *testing.T, dir string, fn func(q store.Querier)) {
	t.Helper()
	db, err := store.Open(context.Background(), "", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.Read(context.Background(), func(q store.Querier) error { fn(q); return nil })
}
