// Command paymux-e2eseed fills a throw-away data directory with payments and
// ledger rows for the dashboard browser tests (frontend/dashboard/e2e). It is
// a separate binary, built only by that test setup: the production binary
// (cmd/paymux) has no flag or command that reaches this code.
//
// Run it after "paymux migrate", "paymux user create" and "paymux app create",
// while no server holds the SQLite lock:
//
//	PAYMUX_DATA_DIR=/tmp/x paymux-e2eseed
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
)

func main() {
	if err := run(context.Background(), os.Getenv("PAYMUX_DATA_DIR")); err != nil {
		fmt.Fprintln(os.Stderr, "paymux-e2eseed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("PAYMUX_DATA_DIR is required")
	}
	db, err := store.Open(ctx, "", dataDir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return err
	}
	return db.Write(ctx, func(tx store.Tx) error {
		apps, err := store.ListApps(ctx, tx, store.DefaultOrgID)
		if err != nil {
			return err
		}
		if len(apps) == 0 {
			return fmt.Errorf("no app: run \"paymux app create\" first")
		}
		app := apps[0]
		now := time.Now().UTC()
		n := 0
		add := func(at time.Time, mode, cur string, amount, refund int64) error {
			n++
			id := fmt.Sprintf("pay_e2e_%03d", n)
			p := store.Payment{ID: id, OrgID: store.DefaultOrgID, AppID: app.ID, Mode: mode, Kind: "one_time", Amount: amount, AmountReceived: amount,
				Currency: cur, Status: "succeeded", OrderRef: fmt.Sprintf("ORD-%03d", n), Metadata: map[string]string{}, ClientSecretHash: "h",
				ExpiresAt: at.Add(time.Hour), Version: 1, Flags: []string{}, CreatedAt: at, UpdatedAt: at}
			if err := store.InsertPayment(ctx, tx, p); err != nil {
				return err
			}
			entries := []store.LedgerEntry{{Kind: "charge", Amount: amount}}
			if refund > 0 {
				entries = append(entries, store.LedgerEntry{Kind: "refund", Amount: -refund})
			}
			for i, e := range entries {
				e.ID, e.OrgID, e.Mode, e.Currency, e.PaymentID, e.OccurredAt = fmt.Sprintf("led_e2e_%03d_%d", n, i), store.DefaultOrgID, mode, cur, id, at
				if err := store.InsertLedgerEntry(ctx, tx, e); err != nil {
					return err
				}
			}
			return nil
		}
		// Fourteen months back, mid-month, in INR (large) and USD (small), so
		// INR stays the default currency and the 12M view has monthly rows.
		for m := 13; m >= 1; m-- {
			at := time.Date(now.Year(), now.Month()-time.Month(m), 15, 9, 0, 0, 0, time.UTC)
			if err := add(at, "live", "INR", 1_200_000+int64(m)*10_000, int64(m%3)*50_000); err != nil {
				return err
			}
			if err := add(at.Add(2*time.Hour), "live", "USD", 4_000+int64(m)*100, 0); err != nil {
				return err
			}
		}
		// The last few days, so 7D and 30D have data, plus one test-mode payment.
		for d := 0; d <= 5; d++ {
			at := now.Add(-time.Duration(d)*24*time.Hour - time.Hour)
			if err := add(at, "live", "INR", 250_000+int64(d)*5_000, 0); err != nil {
				return err
			}
		}
		if err := add(now.Add(-2*time.Hour), "live", "USD", 9_900, 0); err != nil {
			return err
		}
		return add(now.Add(-3*time.Hour), "test", "INR", 77_700, 0)
	})
}
