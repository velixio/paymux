package cli

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
)

func seedPayout(t *testing.T, dir, id, status string) {
	t.Helper()
	ctx := context.Background()
	if code, _, errs := runIn(dir, "migrate"); code != 0 {
		t.Fatalf("migrate: %s", errs)
	}
	db, err := store.Open(ctx, "", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	err = db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Acme", now); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
			return err
		}
		return store.InsertPayout(ctx, tx, store.Payout{ID: id, OrgID: store.DefaultOrgID, Mode: "live", AppID: "app_1", ProviderAccountID: "pa_1",
			ProviderRef: "ref-" + id, Amount: 1000, Currency: "INR", Status: status, BeneficiaryEnc: []byte("x"), BeneficiaryLabel: "••••9012 HDFC",
			Version: 1, CreatedAt: now, UpdatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPayoutResolveSettlesAPayoutInReviewAndAuditsIt(t *testing.T) {
	dir := t.TempDir()
	seedPayout(t, dir, "po_review", "needs_review")
	seedPayout(t, dir, "po_pending", "pending")

	code, out, errs := runIn(dir, "payout", "resolve", "--id", "po_review", "--status", "failed", "--reason", "bank statement shows no debit")
	if code != 0 || !strings.Contains(out, "po_review") || !strings.Contains(out, "failed") {
		t.Fatalf("resolve: %d out=%q err=%q", code, out, errs)
	}
	ctx := context.Background()
	readDB(t, dir, func(q store.Querier) {
		p, err := store.GetPayout(ctx, q, "po_review")
		if err != nil || p.Status != "failed" || p.CompletedAt.IsZero() {
			t.Errorf("payout = %+v %v", p, err)
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'payout.resolve' AND target = 'po_review' AND "after" LIKE '%bank statement shows no debit%'`).Scan(&n); err != nil || n != 1 {
			t.Errorf("audit rows = %d %v", n, err)
		}
	})

	// Only from needs_review; and never without a reason or a settled status.
	for name, args := range map[string][]string{
		"again":      {"--id", "po_review", "--status", "succeeded", "--reason", "x"},
		"pending":    {"--id", "po_pending", "--status", "succeeded", "--reason", "x"},
		"no reason":  {"--id", "po_pending", "--status", "succeeded"},
		"bad status": {"--id", "po_pending", "--status", "processing", "--reason", "x"},
		"no id":      {"--status", "failed", "--reason", "x"},
	} {
		if code, _, errs := runIn(dir, append([]string{"payout", "resolve"}, args...)...); code == 0 {
			t.Errorf("%s: accepted (%q)", name, errs)
		}
	}
	if code, _, _ := runIn(dir, "payout"); code != 2 {
		t.Errorf("no subcommand: %d", code)
	}
}
