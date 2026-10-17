package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/migrations"
)

const (
	taxEntityUsage = `usage: paymux tax-entity add --gstin GSTIN --legal-name NAME [--state CODE] [--trade-name N] [--pan PAN]
           [--address TEXT] [--lut NUMBER --lut-from YYYY-MM-DD --lut-to YYYY-MM-DD]
           [--invoice-prefix INV] [--credit-note-prefix CN] [--signatory NAME] [--footer TEXT] [--support-email EMAIL]`
	appTaxUsage = `usage: paymux app tax --app SLUG --tax-entity ID|GSTIN [--sac 998315] [--rate 1800] [--inclusive=true]
           [--issue-invoices=true] [--invoice-prefix P] [--credit-note-prefix P]`
	reportUsage = `usage: paymux report gst --entity ID|GSTIN --month YYYY-MM --out DIR`
	fxUsage     = `usage: paymux fx set --currency USD --date YYYY-MM-DD --rate 83.2500 [--source manual|provider]`
)

// openInvoice loads the config, opens and migrates the store and builds an
// invoice service that can enqueue jobs but does not run them. On SQLite it
// needs the data-dir lock, so stop "paymux serve" first (or use Postgres).
func openInvoice(ctx context.Context, env func(string) (string, bool), name string, stderr io.Writer) (*invoice.Service, *store.DB, bool) {
	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "paymux %s: %v\n", name, err)
		return nil, nil, false
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		fmt.Fprintf(stderr, "paymux %s: %v\n", name, err)
		return nil, nil, false
	}
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		db.Close()
		fmt.Fprintf(stderr, "paymux %s: %v\n", name, err)
		return nil, nil, false
	}
	return invoice.New(invoice.Options{DB: db, Jobs: jobs.New(db, jobs.Options{})}), db, true
}

func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, tax.IST)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: date %q must be YYYY-MM-DD", invoice.ErrInvalid, s)
	}
	return t, nil
}

func fail(stderr io.Writer, name string, err error) int {
	fmt.Fprintf(stderr, "paymux %s: %v\n", name, err)
	if errors.Is(err, invoice.ErrInvalid) {
		return 2
	}
	return 1
}

// runTaxEntity implements "paymux tax-entity add".
func runTaxEntity(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(stderr, taxEntityUsage)
		return 2
	}
	fs := flag.NewFlagSet("paymux tax-entity add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var in invoice.TaxEntityInput
	var lut, lutFrom, lutTo string
	fs.StringVar(&in.GSTIN, "gstin", "", "GSTIN of the legal entity")
	fs.StringVar(&in.LegalName, "legal-name", "", "legal name")
	fs.StringVar(&in.StateCode, "state", "", "2-digit state code (default: from the GSTIN)")
	fs.StringVar(&in.TradeName, "trade-name", "", "trade name")
	fs.StringVar(&in.PAN, "pan", "", "PAN (default: from the GSTIN)")
	fs.StringVar(&in.Address, "address", "", "address printed on documents")
	fs.StringVar(&lut, "lut", "", "LUT reference number")
	fs.StringVar(&lutFrom, "lut-from", "", "LUT valid from (YYYY-MM-DD)")
	fs.StringVar(&lutTo, "lut-to", "", "LUT valid to (YYYY-MM-DD)")
	fs.StringVar(&in.InvoicePrefix, "invoice-prefix", "", "invoice number prefix (default INV)")
	fs.StringVar(&in.CreditNotePrefix, "credit-note-prefix", "", "credit note number prefix (default CN)")
	fs.StringVar(&in.SignatoryName, "signatory", "", "authorised signatory printed on documents")
	fs.StringVar(&in.FooterNote, "footer", "", "footer note")
	fs.StringVar(&in.SupportEmail, "support-email", "", "support email")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || in.GSTIN == "" || in.LegalName == "" {
		fmt.Fprintln(stderr, taxEntityUsage)
		return 2
	}
	var err error
	in.LUTNumber = lut
	if in.LUTFrom, err = parseDay(lutFrom); err != nil {
		return fail(stderr, "tax-entity add", err)
	}
	if in.LUTTo, err = parseDay(lutTo); err != nil {
		return fail(stderr, "tax-entity add", err)
	}
	svc, db, ok := openInvoice(ctx, env, "tax-entity add", stderr)
	if !ok {
		return 1
	}
	defer db.Close()
	e, err := svc.AddTaxEntity(ctx, cliActor, in)
	if err != nil {
		return fail(stderr, "tax-entity add", err)
	}
	fmt.Fprintf(stdout, "created tax entity %s (%s, GSTIN %s, state %s)\n", e.ID, e.LegalName, e.GSTIN, e.StateCode)
	return 0
}

// runAppTax implements "paymux app tax".
func runAppTax(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("paymux app tax", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var slug, entity string
	in := invoice.AppTaxInput{SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true, IssueInvoices: true}
	fs.StringVar(&slug, "app", "", "slug of the app")
	fs.StringVar(&entity, "tax-entity", "", "tax entity id or GSTIN")
	fs.StringVar(&in.SACCode, "sac", in.SACCode, "SAC code (6 digits)")
	fs.IntVar(&in.GSTRateBP, "rate", in.GSTRateBP, "GST rate in basis points (1800 = 18%)")
	fs.BoolVar(&in.PricesIncludeTax, "inclusive", in.PricesIncludeTax, "prices include tax (--inclusive=false: tax is added)")
	fs.BoolVar(&in.IssueInvoices, "issue-invoices", in.IssueInvoices, "issue tax invoices for this app")
	fs.StringVar(&in.InvoicePrefix, "invoice-prefix", "", "own invoice series prefix for this app")
	fs.StringVar(&in.CreditNotePrefix, "credit-note-prefix", "", "own credit note series prefix for this app")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || slug == "" || entity == "" {
		fmt.Fprintln(stderr, appTaxUsage)
		return 2
	}
	svc, db, ok := openInvoice(ctx, env, "app tax", stderr)
	if !ok {
		return 1
	}
	defer db.Close()
	var app store.App
	err := db.Read(ctx, func(q store.Querier) (err error) {
		app, err = store.GetAppBySlug(ctx, q, store.DefaultOrgID, slug)
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return fail(stderr, "app tax", fmt.Errorf("%w: no app with slug %q", invoice.ErrInvalid, slug))
	}
	if err != nil {
		return fail(stderr, "app tax", err)
	}
	ent, err := svc.FindTaxEntity(ctx, entity)
	if err != nil {
		return fail(stderr, "app tax", err)
	}
	in.AppID, in.TaxEntityID = app.ID, ent.ID
	p, err := svc.SetAppTaxProfile(ctx, cliActor, in)
	if err != nil {
		return fail(stderr, "app tax", err)
	}
	fmt.Fprintf(stdout, "app %s now invoices under %s: issue=%t SAC %s rate %d bp inclusive=%t\n",
		app.Slug, ent.LegalName, p.IssueInvoices, p.SACCode, p.GSTRateBP, p.PricesIncludeTax)
	return 0
}

// runReport implements "paymux report gst".
func runReport(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "gst" {
		fmt.Fprintln(stderr, reportUsage)
		return 2
	}
	fs := flag.NewFlagSet("paymux report gst", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var entity, month, out string
	fs.StringVar(&entity, "entity", "", "tax entity id or GSTIN")
	fs.StringVar(&month, "month", "", "month, YYYY-MM (IST)")
	fs.StringVar(&out, "out", "", "output directory")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || entity == "" || month == "" || out == "" {
		fmt.Fprintln(stderr, reportUsage)
		return 2
	}
	svc, db, ok := openInvoice(ctx, env, "report gst", stderr)
	if !ok {
		return 1
	}
	defer db.Close()
	ent, err := svc.FindTaxEntity(ctx, entity)
	if err != nil {
		return fail(stderr, "report gst", err)
	}
	set, err := svc.Reports(ctx, ent.ID, month)
	if err == nil {
		err = set.Write(out)
	}
	if err != nil {
		return fail(stderr, "report gst", err)
	}
	for _, f := range set.Files {
		fmt.Fprintf(stdout, "wrote %s/%s\n", out, f.Name)
	}
	return 0
}

// runFX implements "paymux fx set": the manual daily FX rate entry.
func runFX(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "set" {
		fmt.Fprintln(stderr, fxUsage)
		return 2
	}
	fs := flag.NewFlagSet("paymux fx set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cur, date, rate, source string
	fs.StringVar(&cur, "currency", "", "ISO 4217 code, e.g. USD")
	fs.StringVar(&date, "date", "", "IST date, YYYY-MM-DD")
	fs.StringVar(&rate, "rate", "", "INR per 1 unit, e.g. 83.2500")
	fs.StringVar(&source, "source", "manual", "manual or provider")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || cur == "" || date == "" || rate == "" {
		fmt.Fprintln(stderr, fxUsage)
		return 2
	}
	svc, db, ok := openInvoice(ctx, env, "fx set", stderr)
	if !ok {
		return 1
	}
	defer db.Close()
	if err := svc.SetFXRate(ctx, cliActor, cur, date, rate, source); err != nil {
		return fail(stderr, "fx set", err)
	}
	fmt.Fprintf(stdout, "stored %s rate %s for %s\n", cur, rate, date)
	return 0
}
