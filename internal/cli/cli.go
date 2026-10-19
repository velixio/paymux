// Package cli implements the paymux command-line entry point.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/hub"
)

const usage = "usage: paymux <serve|migrate|backup|db|build|update|app|provider|domain|tax-entity|report|fx|endpoint|version>"

const help = `usage: paymux [command] [arguments]

With no command, paymux serves (the same as "paymux serve").

Commands:
  serve        run the hub: API, dashboard, webhooks, workers
  migrate      apply database migrations and exit
  backup       write a consistent copy of the database
  db           database tools
  build        build a custom binary with Go hooks and plugins
  update       replace this binary with a newer release
  app          create and manage apps and their API keys
  provider     add and manage provider accounts
  domain       manage app custom domains
  user         manage dashboard users
  endpoint     manage outbound webhook endpoints
  tax-entity   manage the GST tax entity
  report       export ledger and tax reports
  fx           record exchange rates
  version      print the version

Configuration comes from the environment and .env (PAYMUX_ENV_FILE).
Flags:
  -h, --help   show this help
`

// Run executes the CLI and returns the process exit code.
func Run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	return RunWith(ctx, args, lookupEnv, stdout, stderr, nil)
}

// RunWith is Run for a custom binary (framework mode): "serve" calls each
// setup function on the App, in order, before starting it, which is where
// hooks are registered.
func RunWith(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer, setup []func(*hub.App)) int {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, help)
		return 0
	case "version":
		fmt.Fprintf(stdout, "paymux %s\n", hub.Version)
		return 0
	case "db":
		return runDB(ctx, args[1:], stdout, stderr)
	case "build":
		return runBuild(ctx, args[1:], stdout, stderr)
	case "update":
		return runUpdate(ctx, args[1:], lookupEnv, stdout, stderr)
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
	case "serve", "migrate", "backup", "app", "provider", "domain":
		env, err := envWithDotEnv(lookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "paymux %s: %v\n", args[0], err)
			return 1
		}
		switch args[0] {
		case "migrate":
			return runMigrate(ctx, env, stdout, stderr)
		case "backup":
			return runBackup(ctx, args[1:], env, stdout, stderr)
		case "app":
			return runApp(ctx, args[1:], env, stdout, stderr)
		case "provider":
			return runProvider(ctx, args[1:], env, stdout, stderr)
		case "domain":
			return runDomain(ctx, args[1:], env, stdout, stderr)
		}
		return runServe(ctx, env, stderr, setup)
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

func runServe(ctx context.Context, env func(string) (string, bool), stderr io.Writer, setup []func(*hub.App)) int {
	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "paymux serve: invalid configuration:\n%v\n", err)
		return 1
	}
	app, err := hub.New(cfg)
	if err == nil {
		for _, f := range setup {
			f(app)
		}
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
