package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

const appUsage = `usage: paymux app create --name NAME --slug SLUG [--mode test|live] [--return-url URL]
       paymux app key --app SLUG [--mode test|live] [--scopes payments:read,refunds:write]
       paymux app key --app SLUG --rotate [--mode test|live] [--grace-hours 24] [--scopes ...]
       paymux app tax --app SLUG --tax-entity ID|GSTIN [--sac 998315] [--rate 1800] [--inclusive=true]`

// cliActor attributes CLI changes in the audit log.
var cliActor = audit.Actor{Type: audit.ActorSystem, ID: "cli"}

// runApp implements "paymux app create" and "paymux app key". On SQLite it
// needs the data-dir lock, so stop "paymux serve" first (or use Postgres).
func runApp(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "tax" {
		return runAppTax(ctx, args[1:], env, stdout, stderr)
	}
	if len(args) == 0 || (args[0] != "create" && args[0] != "key") {
		fmt.Fprintln(stderr, appUsage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("paymux app "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var name, slug, appSlug, mode, returnURL, scopes string
	var rotate bool
	var graceHours int
	fs.StringVar(&mode, "mode", "test", "key mode: test or live")
	fs.StringVar(&scopes, "scopes", "", "comma-separated scopes (default: all), e.g. payments:read,refunds:write")
	if sub == "create" {
		fs.StringVar(&name, "name", "", "app name")
		fs.StringVar(&slug, "slug", "", "app slug")
		fs.StringVar(&returnURL, "return-url", "", "default return URL")
	} else {
		fs.StringVar(&appSlug, "app", "", "slug of the app")
		fs.BoolVar(&rotate, "rotate", false, "create a new key and let the live keys of this mode expire after --grace-hours")
		fs.IntVar(&graceHours, "grace-hours", 24, "with --rotate: hours the old keys keep working")
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

	keyOpts := core.KeyOptions{}
	if scopes != "" {
		keyOpts.Scopes = strings.Split(scopes, ",")
	}
	if rotate && graceHours < 1 {
		fmt.Fprintf(stderr, "paymux app key: --grace-hours must be at least 1\n")
		return 1
	}
	rot := time.Duration(0)
	if rotate {
		rot = time.Duration(graceHours) * time.Hour
	}
	if err := runAppCommand(ctx, db, sub, name, slug, appSlug, mode, returnURL, keyOpts, rot, stdout); err != nil {
		fmt.Fprintf(stderr, "paymux app %s: %v\n", sub, err)
		return 1
	}
	return 0
}

// runAppCommand creates the app (sub "create") and a key for it. rotate > 0
// (sub "key" only) makes the key a rotation with that grace period.
func runAppCommand(ctx context.Context, db *store.DB, sub, name, slug, appSlug, mode, returnURL string, keyOpts core.KeyOptions, rotate time.Duration, stdout io.Writer) error {
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
	if rotate > 0 {
		key, nk, expiring, err := svc.RotateAPIKey(ctx, cliActor, sc, app.ID, mode, rotate, keyOpts)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s API key for %s, shown only once (store it now):\n%s\n", mode, app.Slug, key)
		fmt.Fprintf(stdout, "scopes: %s\n", scopesText(nk.Scopes))
		for _, k := range expiring {
			fmt.Fprintf(stdout, "previous key %s… stops working at %s\n", k.Prefix, k.ExpiresAt.UTC().Format(time.RFC3339))
		}
		if len(expiring) == 0 {
			fmt.Fprintln(stdout, "there was no previous key to expire")
		}
		return nil
	}
	key, nk, err := svc.CreateAPIKeyWith(ctx, cliActor, sc, app.ID, mode, keyOpts)
	if err != nil {
		if sub == "create" {
			err = errors.Join(err, fmt.Errorf("the app was created; run \"paymux app key --app %s --mode %s\" to get a key", app.Slug, mode))
		}
		return err
	}
	fmt.Fprintf(stdout, "%s API key for %s, shown only once (store it now):\n%s\n", mode, app.Slug, key)
	if nk.Scopes != nil {
		fmt.Fprintf(stdout, "scopes: %s\n", scopesText(nk.Scopes))
	}
	return nil
}

func scopesText(scopes []string) string {
	if scopes == nil {
		return "all"
	}
	return strings.Join(scopes, ",")
}
