package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
)

const appUsage = `usage: paymux app create --name NAME --slug SLUG [--mode test|live] [--return-url URL]
       paymux app key --app SLUG [--mode test|live]`

// cliActor attributes CLI changes in the audit log.
var cliActor = audit.Actor{Type: audit.ActorSystem, ID: "cli"}

// runApp implements "paymux app create" and "paymux app key". On SQLite it
// needs the data-dir lock, so stop "paymux serve" first (or use Postgres).
func runApp(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "create" && args[0] != "key") {
		fmt.Fprintln(stderr, appUsage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("paymux app "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var name, slug, appSlug, mode, returnURL string
	fs.StringVar(&mode, "mode", "test", "key mode: test or live")
	if sub == "create" {
		fs.StringVar(&name, "name", "", "app name")
		fs.StringVar(&slug, "slug", "", "app slug")
		fs.StringVar(&returnURL, "return-url", "", "default return URL")
	} else {
		fs.StringVar(&appSlug, "app", "", "slug of the app")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || (sub == "create" && (name == "" || slug == "")) || (sub == "key" && appSlug == "") {
		fmt.Fprintln(stderr, appUsage)
		return 2
	}
	if mode != "test" && mode != "live" {
		fmt.Fprintf(stderr, "paymux app %s: --mode must be test or live\n", sub)
		return 1
	}

	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "paymux app %s: %v\n", sub, err)
		return 1
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		fmt.Fprintf(stderr, "paymux app %s: %v\n", sub, err)
		return 1
	}
	defer db.Close()

	if err := runAppCommand(ctx, db, sub, name, slug, appSlug, mode, returnURL, stdout); err != nil {
		fmt.Fprintf(stderr, "paymux app %s: %v\n", sub, err)
		return 1
	}
	return 0
}

func runAppCommand(ctx context.Context, db *store.DB, sub, name, slug, appSlug, mode, returnURL string, stdout io.Writer) error {
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return err
	}
	set := settings.New(db)
	org, err := core.Bootstrap(ctx, db, set)
	if err != nil {
		return err
	}
	sc := core.OrgScope{OrgID: org}
	svc := core.NewService(core.Options{DB: db, Settings: set})

	var app store.App
	if sub == "create" {
		if app, err = svc.CreateApp(ctx, cliActor, sc, core.CreateAppInput{Name: name, Slug: slug, DefaultReturnURL: returnURL}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "created app %s (slug %s, id %s)\n", app.Name, app.Slug, app.ID)
	} else if app, err = svc.AppBySlug(ctx, sc, appSlug); err != nil {
		return err
	}
	key, _, err := svc.CreateAPIKey(ctx, cliActor, sc, app.ID, mode)
	if err != nil {
		if sub == "create" {
			err = errors.Join(err, fmt.Errorf("the app was created; run \"paymux app key --app %s --mode %s\" to get a key", app.Slug, mode))
		}
		return err
	}
	fmt.Fprintf(stdout, "%s API key for %s, shown only once (store it now):\n%s\n", mode, app.Slug, key)
	return nil
}
