package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
)

const (
	providerUsage = "usage: paymux provider add --connector NAME --name NAME --mode test|live --config-file FILE [--currencies INR,USD] [--app APP]"
	domainUsage   = "usage: paymux domain add --app APP --host HOST"
)

// cliContext is what a management command needs: a migrated, bootstrapped
// database and the services on it. With SQLite the command needs the
// data-dir lock, so stop "paymux serve" first (or use Postgres).
type cliContext struct {
	cfg   config.Config
	db    *store.DB
	svc   *core.Service
	conns *connectors.Service
	scope core.OrgScope
}

func openCLI(ctx context.Context, env func(string) (string, bool), stderr io.Writer) (*cliContext, error) {
	cfg, err := config.Load(env)
	if err != nil {
		return nil, err
	}
	box, err := secrets.NewBox(cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*cliContext, error) { db.Close(); return nil, err }
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return fail(err)
	}
	set := settings.New(db)
	org, err := core.Bootstrap(ctx, db, set)
	if err != nil {
		return fail(err)
	}
	return &cliContext{cfg: cfg, db: db, svc: core.NewService(core.Options{DB: db, Settings: set}),
		conns: connectors.New(connectors.Options{DB: db, Box: box, BaseURL: cfg.BaseURL}), scope: core.OrgScope{OrgID: org}}, nil
}

// resolveApp finds an app by slug, or by id.
func (c *cliContext) resolveApp(ctx context.Context, ref string) (store.App, error) {
	app, err := c.svc.AppBySlug(ctx, c.scope, ref)
	if errors.Is(err, core.ErrNotFound) {
		if byID, gerr := c.svc.GetApp(ctx, ref); gerr == nil && byID.OrgID == c.scope.OrgID {
			return byID, nil
		}
	}
	return app, err
}

func runProvider(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(stderr, providerUsage)
		return 2
	}
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	conn := fs.String("connector", "", "connector name, e.g. razorpay")
	name := fs.String("name", "", "account name")
	mode := fs.String("mode", "", "test or live")
	cfgFile := fs.String("config-file", "", "JSON file with the connector config")
	currencies := fs.String("currencies", "", "comma-separated currencies (default: all the connector supports)")
	app := fs.String("app", "", "app (slug or id) allowed to use this account")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *conn == "" || *name == "" || *mode == "" || *cfgFile == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, providerUsage)
		return 2
	}
	if *mode != "test" && *mode != "live" {
		fmt.Fprintln(stderr, "paymux provider add: --mode must be test or live")
		return 1
	}
	raw, err := os.ReadFile(*cfgFile)
	if err != nil {
		fmt.Fprintf(stderr, "paymux provider add: cannot read config file: %v\n", err)
		return 1
	}
	if !json.Valid(raw) {
		fmt.Fprintln(stderr, "paymux provider add: config file is not valid JSON")
		return 1
	}
	var cur []string
	for _, c := range strings.Split(*currencies, ",") {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			cur = append(cur, c)
		}
	}

	c, err := openCLI(ctx, env, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "paymux provider add: %v\n", err)
		return 1
	}
	defer c.db.Close()
	in := connectors.CreateAccountInput{Connector: *conn, Name: *name, Mode: *mode, Config: raw, Currencies: cur}
	if *app != "" {
		a, err := c.resolveApp(ctx, *app)
		if err != nil {
			fmt.Fprintf(stderr, "paymux provider add: app %q: %v\n", *app, err)
			return 1
		}
		in.AppID = a.ID
	}
	pa, err := c.conns.CreateAccount(ctx, cliActor, c.scope, in)
	if err != nil {
		fmt.Fprintf(stderr, "paymux provider add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created %s provider account %q (%s, %s)\n", pa.Connector, pa.Name, pa.ID, pa.Mode)
	fmt.Fprintf(stdout, "Register this URL as the webhook in the provider dashboard:\n%s\n", c.conns.WebhookURL(pa.ID))
	if in.AppID == "" {
		fmt.Fprintln(stdout, "note: this account is not linked to any app yet, so no payment can use it; re-run with --app.")
	}
	return 0
}

func runDomain(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(stderr, domainUsage)
		return 2
	}
	fs := flag.NewFlagSet("domain add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "app (slug or id)")
	host := fs.String("host", "", "custom host name, e.g. pay.example.com")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *app == "" || *host == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, domainUsage)
		return 2
	}
	c, err := openCLI(ctx, env, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "paymux domain add: %v\n", err)
		return 1
	}
	defer c.db.Close()
	a, err := c.resolveApp(ctx, *app)
	if err != nil {
		fmt.Fprintf(stderr, "paymux domain add: app %q: %v\n", *app, err)
		return 1
	}
	if strings.EqualFold(strings.TrimSuffix(*host, "."), c.cfg.BaseURL.Hostname()) {
		fmt.Fprintln(stderr, "paymux domain add: that is the hub's own host, not a custom domain")
		return 1
	}
	d, err := c.svc.AddDomain(ctx, cliActor, c.scope, a.ID, *host)
	if err != nil {
		fmt.Fprintf(stderr, "paymux domain add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added domain %s to %s (status %s%s)\n", d.Host, a.Slug, d.Status, map[bool]string{true: ", primary"}[d.IsPrimary])
	fmt.Fprintf(stdout, "Point its DNS at this server, then it must serve https://%s/.well-known/paymux/%s\n", d.Host, d.Token)
	fmt.Fprintln(stdout, "Remember to register the domain in each provider's website whitelist.")
	return 0
}
