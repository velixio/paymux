package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/reseal"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
)

const secretsUsage = "usage: paymux secrets reseal [--dry-run]\n\n" +
	"Rewrites every sealed value (provider credentials, webhook signing secrets, authenticator secrets,\n" +
	"payout beneficiaries, signing certificates, connector state, stored API responses) under the current\n" +
	"PAYMUX_MASTER_KEY. Earlier keys go in PAYMUX_MASTER_KEY_OLD (comma separated) or PAYMUX_MASTER_KEY_OLD_FILE.\n" +
	"It refuses, and changes nothing, if any value cannot be opened. It is safe to run again."

// runSecrets handles "paymux secrets ...". Like the other commands that open
// the database, it cannot run on SQLite while "paymux serve" holds the lock;
// on PostgreSQL it can run beside the hub.
func runSecrets(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "reseal" {
		fmt.Fprintln(stderr, secretsUsage)
		return 2
	}
	fs := flag.NewFlagSet("secrets reseal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dry := fs.Bool("dry-run", false, "count what would be resealed and change nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, secretsUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "paymux secrets reseal: %v%s\n", err, lockedHint(err))
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
	res, err := reseal.Run(ctx, db, box, *dry)
	if err != nil {
		return fail(err)
	}
	verb := "resealed"
	if *dry {
		verb = "would reseal"
	}
	fmt.Fprintf(stdout, "%s %d sealed values (%d were already under the current key, %s)\n", verb, res.Resealed, res.Current, secrets.KeyID(cfg.MasterKey))
	if !*dry && res.Resealed == 0 && len(cfg.OldMasterKeys) > 0 {
		fmt.Fprintln(stdout, "nothing is sealed under an old key any more: PAYMUX_MASTER_KEY_OLD can be removed")
	}
	return 0
}
