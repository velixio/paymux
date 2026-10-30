package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"text/tabwriter"

	"github.com/velixio/paymux/internal/config"
	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
)

const backupUsage = "usage: paymux backup --to <file>"

const dbUsage = "usage: paymux db copy --from <database-url> --to <database-url> [--force]"

func lockedHint(err error) string {
	if errors.Is(err, store.ErrLocked) {
		return "\nstop \"paymux serve\" first: a SQLite database is locked by the running server"
	}
	return ""
}

// runBackup handles "paymux backup --to <file>". With SQLite it writes a
// consistent standalone copy (VACUUM INTO); PostgreSQL is backed up with
// pg_dump and the command says so.
func runBackup(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "file to write the backup to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, backupUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "paymux backup: %v%s\n", err, lockedHint(err))
		return 1
	}
	cfg, err := config.Load(env)
	if err != nil {
		return fail(err)
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	if err := db.Backup(ctx, *to); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "backup written to %s\n", *to)
	return 0
}

// runDB handles "paymux db copy": copy every table from one database to
// another, across dialects, and verify row counts and checksums.
func runDB(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "copy" {
		fmt.Fprintln(stderr, dbUsage)
		return 2
	}
	fs := flag.NewFlagSet("db copy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "source database URL")
	to := fs.String("to", "", "target database URL")
	force := fs.Bool("force", false, "empty a target that already has data first")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *from == "" || *to == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, dbUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "paymux db copy: %v%s\n", err, lockedHint(err))
		return 1
	}
	if *from == *to {
		return fail(errors.New("--from and --to are the same database"))
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	src, err := store.Open(ctx, *from, "", logger)
	if err != nil {
		return fail(err)
	}
	defer src.Close()
	dst, err := store.Open(ctx, *to, "", logger)
	if err != nil {
		return fail(err)
	}
	defer dst.Close()
	if err := dst.Migrate(ctx, migrations.FS); err != nil {
		return fail(fmt.Errorf("migrating the target: %w", err))
	}
	rep, err := store.CopyAll(ctx, src, dst, store.CopyOptions{Force: *force})
	if err != nil {
		return fail(err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TABLE\tROWS\tCHECKSUM\tVERIFIED")
	for _, t := range rep.Tables {
		fmt.Fprintf(tw, "%s\t%d\t%s\tok\n", t.Table, t.Rows, t.TargetChecksum)
	}
	fmt.Fprintf(tw, "total\t%d\t\tok\n", rep.Rows)
	_ = tw.Flush()
	return 0
}
