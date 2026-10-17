// Package cli implements the paymux command-line entry point.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/paymux/paymux"
	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
)

const usage = "usage: paymux <serve|migrate|app|tax-entity|report|fx|endpoint|version>"

// Run executes the CLI and returns the process exit code.
func Run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "paymux %s\n", paymux.Version)
		return 0
	case "user":
		env, err := envWithDotEnv(lookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "paymux user: %v\n", err)
			return 1
		}
		return runUser(ctx, args[1:], env, stdout, stderr)
	case "endpoint":
		env, err := envWithDotEnv(lookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "paymux endpoint: %v\n", err)
			return 1
		}
		return runEndpoint(ctx, args[1:], env, stdout, stderr)
	case "tax-entity", "report", "fx":
		env, err := envWithDotEnv(lookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "paymux %s: %v\n", args[0], err)
			return 1
		}
		switch args[0] {
		case "tax-entity":
			return runTaxEntity(ctx, args[1:], env, stdout, stderr)
		case "report":
			return runReport(ctx, args[1:], env, stdout, stderr)
		}
		return runFX(ctx, args[1:], env, stdout, stderr)
	case "serve", "migrate", "app":
		env, err := envWithDotEnv(lookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "paymux %s: %v\n", args[0], err)
			return 1
		}
		switch args[0] {
		case "migrate":
			return runMigrate(ctx, env, stdout, stderr)
		case "app":
			return runApp(ctx, args[1:], env, stdout, stderr)
		}
		return runServe(ctx, env, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// envWithDotEnv layers the .env file (PAYMUX_ENV_FILE, default ".env") under
// the real environment.
func envWithDotEnv(lookupEnv func(string) (string, bool)) (func(string) (string, bool), error) {
	path := ".env"
	if v, ok := lookupEnv("PAYMUX_ENV_FILE"); ok && v != "" {
		path = v
	}
	file, err := config.LoadDotEnv(path)
	if err != nil {
		return nil, err
	}
	return config.WithDotEnv(lookupEnv, file), nil
}

func runServe(ctx context.Context, env func(string) (string, bool), stderr io.Writer) int {
	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "paymux serve: invalid configuration:\n%v\n", err)
		return 1
	}
	app, err := paymux.New(cfg)
	if err == nil {
		err = app.Start(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "paymux serve: %v\n", err)
		return 1
	}
	return 0
}

func runMigrate(ctx context.Context, env func(string) (string, bool), stdout, stderr io.Writer) int {
	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "paymux migrate: %v\n", err)
		return 1
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		fmt.Fprintf(stderr, "paymux migrate: %v\n", err)
		return 1
	}
	defer db.Close()
	n, err := db.PendingMigrations(ctx, migrations.FS)
	if err == nil {
		err = db.Migrate(ctx, migrations.FS)
	}
	if err != nil {
		fmt.Fprintf(stderr, "paymux migrate: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "applied %d migrations (%s)\n", n, db.Dialect().Name())
	return 0
}
