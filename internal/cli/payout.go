package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/payouts"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
)

const payoutUsage = "usage: paymux payout resolve --id po_... --status succeeded|failed --reason TEXT"

// runPayout handles "paymux payout ...". Like the other commands that open the
// database, it cannot run on SQLite while "paymux serve" holds the lock.
func runPayout(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "resolve" {
		fmt.Fprintln(stderr, payoutUsage)
		return 2
	}
	fs := flag.NewFlagSet("payout resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "the payout, in status needs_review")
	status := fs.String("status", "", "what the provider's records show: succeeded or failed")
	reason := fs.String("reason", "", "why (what you checked); kept in the audit log")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *id == "" || *status == "" || *reason == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, payoutUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "paymux payout resolve: %v\n", err)
		if errors.Is(err, payouts.ErrInvalid) || errors.Is(err, payouts.ErrNotFound) {
			return 2
		}
		return 1
	}
	cfg, err := config.Load(env)
	if err != nil {
		return fail(err)
	}
	box, err := secrets.NewBox(cfg.MasterKey, cfg.OldMasterKeys...)
	if err != nil {
		return fail(err)
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return fail(err)
	}
	// The event is queued for delivery; the running hub sends it.
	q := jobs.New(db, jobs.Options{})
	svc := payouts.New(payouts.Options{DB: db, Box: box, Jobs: q, Logger: slog.New(slog.NewTextHandler(stderr, nil)),
		Emitter: outbound.New(outbound.Options{DB: db, Queue: q, Box: box, AllowPrivate: cfg.AllowPrivateWebhooks})})
	p, err := svc.Resolve(ctx, *id, *status, *reason, cliActor)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "payout %s resolved as %s\n", p.ID, p.Status)
	return 0
}
