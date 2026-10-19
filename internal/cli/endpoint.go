package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
)

const endpointUsage = "usage: paymux endpoint add --app <app_id> --url <url> [--events a,b] [--mode test|live]"

// runEndpoint handles "paymux endpoint ...". Like migrate, it opens the
// database directly, so with SQLite it cannot run while "paymux serve" holds
// the data-dir lock.
func runEndpoint(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(stderr, endpointUsage)
		return 2
	}
	fs := flag.NewFlagSet("endpoint add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	app := fs.String("app", "", "app id the endpoint belongs to")
	url := fs.String("url", "", "URL that receives webhooks")
	events := fs.String("events", "", "comma-separated event types (default: all)")
	mode := fs.String("mode", "live", "test or live: the endpoint receives the events of that mode only")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *app == "" || *url == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, endpointUsage)
		return 2
	}
	var types []string
	for _, t := range strings.Split(*events, ",") {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}

	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "paymux endpoint add: %v\n", err)
		return 1
	}
	box, err := secrets.NewBox(cfg.MasterKey, cfg.OldMasterKeys...)
	if err != nil {
		fmt.Fprintf(stderr, "paymux endpoint add: %v\n", err)
		return 1
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		fmt.Fprintf(stderr, "paymux endpoint add: %v\n", err)
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		fmt.Fprintf(stderr, "paymux endpoint add: %v\n", err)
		return 1
	}

	svc := outbound.New(outbound.Options{DB: db, Box: box})
	ep, secret, err := svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{AppID: *app, URL: *url, EventTypes: types, Mode: *mode})
	if err != nil {
		fmt.Fprintf(stderr, "paymux endpoint add: %v\n", err)
		return 1
	}
	all := "all"
	if len(ep.EventTypes) > 0 {
		all = strings.Join(ep.EventTypes, ",")
	}
	fmt.Fprintf(stdout, "endpoint %s created for app %s\nurl:    %s\nmode:   %s\nevents: %s\n\nsigning secret (shown once, store it now):\n%s\n",
		ep.ID, ep.AppID, ep.URL, ep.Mode, all, secret)
	return 0
}
