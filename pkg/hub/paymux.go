// Package hub is the Paymux payments hub: the application cmd/paymux runs and
// that framework mode embeds (New, Run, hooks, the JS runtime).
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/docs"
	"github.com/paymux/paymux/internal/alertcenter"
	"github.com/paymux/paymux/internal/apidocs"
	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/clientip"
	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/dashboard"
	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/disputes"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/housekeeping"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/metrics"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/oidc"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/payouts"
	"github.com/paymux/paymux/internal/paypage"
	"github.com/paymux/paymux/internal/reconcile"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/selfupdate"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector"
)

// Version is the build version; overridden via -ldflags -X.
var Version = "dev"

const (
	heartbeatKind     = "system.heartbeat"
	heartbeatEvery    = time.Minute
	heartbeatGrace    = 3 * time.Minute // max age of the last run once uptime exceeds it
	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second  // the whole request, body included
	writeTimeout      = 120 * time.Second // the whole response (a log export or an invoice PDF is the longest)
	idleTimeout       = 120 * time.Second // between requests on a kept-alive connection
	maxHeaderBytes    = 64 << 10
)

// newServer is an http.Server with every timeout set, so that a client sending
// or reading a byte a minute cannot hold a connection (and its goroutine and
// buffers) for ever.
func newServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout,
		WriteTimeout: writeTimeout, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes}
}

// App is a Paymux instance.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	emitter events.Emitter // nil means the outbound service

	verifiers inbound.VerifierFunc
	connHTTP  func(store.ProviderAccount, connector.Info, []string) *http.Client

	hooks      hookSet                                                         // Go hooks (hooks.go)
	hookFailed func(ctx context.Context, hook string, ev hookEvent, err error) // set by Start unless a test sets it

	mu          sync.Mutex
	addr, iaddr string
	logDB       *store.DB // the database log (dblogs.go)
	logRec      *dblog.Recorder
	selfUpdate  *selfUpdateConfig
	restart     bool
	cancelRun   context.CancelFunc
}

// WithVerifiers overrides how inbound webhooks find the Verifier of a provider
// account. The default is each account's connector (VerifyWebhook).
func WithVerifiers(f inbound.VerifierFunc) Option { return func(a *App) { a.verifiers = f } }

// Option customises an App.
type Option func(*App)

// WithLogger sets the logger. The default writes redacted JSON to stderr.
func WithLogger(l *slog.Logger) Option { return func(a *App) { a.log = l } }

// WithConnectorHTTP overrides the HTTP client given to each connector (tests
// point connectors at a fake provider). The default is
// connector.NewHTTPClient with per-account rate limiting and log redaction.
func WithConnectorHTTP(f func(pa store.ProviderAccount, info connector.Info, secretValues []string) *http.Client) Option {
	return func(a *App) { a.connHTTP = f }
}

// WithEmitter replaces the domain-event emitter. The default is the outbound
// webhooks service.
func WithEmitter(e events.Emitter) Option { return func(a *App) { a.emitter = e } }

// New builds an App. Nothing is opened until Start.
func New(cfg config.Config, opts ...Option) (*App, error) {
	if cfg.DatabaseURL == "" {
		return nil, errors.New("paymux: config has no DatabaseURL")
	}
	if cfg.DevSkip2FA && cfg.Env != config.EnvDevelopment {
		return nil, errors.New("paymux: PAYMUX_DEV_SKIP_2FA is only allowed in development (PAYMUX_ENV=development)")
	}
	a := &App{cfg: cfg}
	for _, o := range opts {
		o(a)
	}
	if a.log == nil {
		a.log = logging.New(os.Stderr, slog.LevelInfo)
	}
	return a, nil
}

// Addr is the bound address of the main listener, or "" before Start binds it.
func (a *App) Addr() string { a.mu.Lock(); defer a.mu.Unlock(); return a.addr }

// InternalAddr is the bound address of the internal listener, or "".
func (a *App) InternalAddr() string { a.mu.Lock(); defer a.mu.Unlock(); return a.iaddr }

// EnableSelfUpdate turns on the dashboard's Update page for the stock paymux
// binary: it installs releases of repo (owner/name) over the binary at exe and
// then restarts the process, so Start returns and the caller starts exe again
// (see RestartRequested). A custom binary built on this package does not call
// it: replacing it with the stock paymux would be wrong.
func (a *App) EnableSelfUpdate(repo, exe string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.selfUpdate = &selfUpdateConfig{repo: repo, exe: exe}
}

// RestartRequested reports that Start returned because an update was installed
// and the process should start the new binary.
func (a *App) RestartRequested() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restart
}

// requestRestart makes Start stop gracefully, as on a shutdown signal, and
// marks the stop as a restart.
func (a *App) requestRestart() {
	a.mu.Lock()
	a.restart = true
	cancel := a.cancelRun
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

type selfUpdateConfig struct{ repo, exe string }

// Start opens the store, migrates, starts the job queue and both listeners,
// and blocks until ctx is cancelled (returning nil after a graceful stop) or
// something fails.
func (a *App) Start(ctx context.Context) error {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	a.mu.Lock()
	a.cancelRun = cancelRun
	a.mu.Unlock()
	db, err := store.Open(ctx, a.cfg.DatabaseURL, a.cfg.DataDir, a.log)
	if err != nil {
		return err
	}
	defer db.Close()
	a.warnDatabaseTLS()
	if msg := config.CheckoutURLWarning(a.cfg); msg != "" {
		a.log.Warn(msg)
	}
	a.warnLooseFiles(db)

	dialect := db.Dialect().Name()
	a.log.Info("paymux starting", "version", Version, "dialect", dialect,
		"database", a.cfg.RedactedDatabaseURL(), "env", string(a.cfg.Env))
	if a.cfg.DevSkip2FA {
		if host := a.cfg.BaseURL.Hostname(); !config.IsLocalHost(host) {
			a.log.Warn("2FA is disabled on a public host: anyone with a password can sign in; set PAYMUX_ENV=production before going live", "host", host)
		} else {
			a.log.Warn("2FA is disabled (development)")
		}
	}

	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return err
	}
	// Warnings and errors are also kept in the database for later review. From
	// here on a.log writes to both stdout and that log.
	dbl := a.openDBLogs(ctx, db)
	defer dbl.close()

	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	box, err := secrets.NewBox(a.cfg.MasterKey, a.cfg.OldMasterKeys...)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}

	started := time.Now()
	q := jobs.New(db, jobs.Options{Logger: a.log})
	q.Register(heartbeatKind, func(context.Context, json.RawMessage) error { return nil })
	if err := q.AddSchedule(jobs.Schedule{Name: heartbeatKind, Kind: heartbeatKind, Every: heartbeatEvery}); err != nil {
		return err
	}

	// The outbound service is the events.Emitter the domain services emit
	// through, unless WithEmitter swapped it.
	out := outbound.New(outbound.Options{DB: db, Queue: q, Box: box,
		AllowPrivate: a.cfg.AllowPrivateWebhooks, Logger: a.log})
	out.Register()
	var emitter events.Emitter = out
	if a.emitter != nil {
		emitter = a.emitter
	}
	// Go hooks run after commit: every event is also queued as a job in the
	// same transaction (only when a hook listens to it).
	emitter = hookEmitter{next: emitter, queue: q, hooks: &a.hooks}
	breaker := routing.NewBreaker(nil)
	rt := checkout.NewRouting(breaker).WithSettings(set).WithSuggester(a.suggestRoute)
	reg := metrics.NewRegistry()
	// Every URL a customer sees (pay page, links, invoices, subscription
	// checkouts, return pages) is built on the checkout origin when there is one;
	// webhooks, the API and the dashboard stay on the base URL.
	customerURL := a.cfg.CustomerURL().String()
	connOpts := connectors.Options{DB: db, Box: box, BaseURL: a.cfg.BaseURL, CheckoutURL: a.cfg.CheckoutURL, Logger: a.log, HTTPClient: a.connHTTP,
		Observe: connectors.ProviderMetrics(reg)}
	if dbl != nil {
		connOpts.CallRecorder = func(pa store.ProviderAccount, info connector.Info) connector.CallRecorder {
			return dbl.rec.ProviderCall(dblog.ProviderMeta{Connector: pa.Connector, AccountID: pa.ID, OrgID: pa.OrgID})
		}
		connOpts.KnownSecrets = func(owner string, values []string) { dbl.red.SetKnown(owner, values...) }
		dbl.rec.RegisterMetrics(reg)
	}
	conns := connectors.New(connOpts)
	if err := conns.LoadKnownSecrets(ctx); err != nil {
		a.log.Warn("could not load provider secrets for log redaction", "err", err.Error())
	}
	// GST invoicing: documents are issued by hooks inside the transaction in
	// which a payment or refund succeeds.
	mailer := notify.New(a.cfg)
	inv := invoice.New(invoice.Options{DB: db, Box: box, Settings: set, Jobs: q, Emitter: emitter, Mailer: mailer,
		BaseURL: customerURL})
	inv.RegisterJobs(q)
	// Ops alerts (PAYMUX_OPS_EMAILS and PAYMUX_OPS_WEBHOOK_URL) go through one
	// rate-limited sender shared by every component that raises them.
	// Every alert is also kept in the alerts table, for the dashboard's Alerts page.
	alertCenter := alertcenter.New(db, nil)
	alerts := notify.NewAlerts(notify.AlertsOptions{Mailer: mailer, Emails: a.cfg.OpsEmails, WebhookURL: a.cfg.OpsWebhookURL,
		Recorder: alertCenter, Logger: a.log})
	a.registerHookJob(q, alerts)
	// A live account holding sandbox credentials (or a test one holding live
	// credentials) is disabled and reported; Get refuses it in any case. The hub
	// keeps running without it.
	conns.SetAlerter(alerts)
	if n, err := conns.DisableSandboxOnLive(ctx); err != nil {
		a.log.Error("could not check the provider accounts against their modes", "err", err.Error())
	} else if n > 0 {
		a.log.Error("provider accounts disabled: their stored config does not belong to their mode", "accounts", n)
	}
	inv.SetAlerter(alerts) // tax exceptions (pos_defaulted, export_igst, fx_pending, credit-note cutoff)
	// A payment that succeeds issues its invoice and spends the single-use
	// payment link it came from, in the same transaction.
	var links *paylinks.Service
	onPaymentSucceeded := func(ctx context.Context, tx store.Tx, p store.Payment, at store.Attempt) error {
		if err := inv.OnPaymentSucceeded(ctx, tx, p, at); err != nil {
			return err
		}
		return links.OnPaymentSucceeded(ctx, tx, p, at)
	}
	svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: q, Emitter: emitter, Router: rt, Alerts: alerts,
		OnPaymentSucceeded: onPaymentSucceeded, OnRefundSucceeded: inv.OnRefundSucceeded})
	links = paylinks.New(paylinks.Options{DB: db, Core: svc, Emitter: emitter, Settings: set, BaseURL: customerURL})
	disp := disputes.New(disputes.Options{DB: db, Settings: set, Emitter: emitter, Alerts: alerts, Credit: inv, Conns: conns, Core: svc,
		Logger: a.log, CutoffErr: invoice.ErrCreditNoteCutoff})
	// Subscriptions and mandates: renewals run as jobs; provider notifications
	// and returns reach the engine through the payment flow.
	subCSRF := sha256.Sum256(append([]byte("paymux-sub-csrf-key|"), a.cfg.MasterKey...))
	subs := subscriptions.New(subscriptions.Options{DB: db, Core: svc, Settings: set, Jobs: q, Emitter: emitter, CSRFKey: subCSRF[:],
		BaseURL: customerURL, Mailer: notify.New(a.cfg), Logger: a.log,
		Connectors: subscriptions.ConnectorsFunc(func(ctx context.Context, id string) (connector.Connector, error) {
			live, err := conns.Get(ctx, id)
			return live.Connector, err
		})})
	subs.RegisterJobs(q)
	payoutSvc := payouts.New(payouts.Options{DB: db, Box: box, Jobs: q, Emitter: emitter, Logger: a.log, Alerts: alerts,
		Connectors: payouts.ConnectorsFunc(func(ctx context.Context, id string) (connector.Connector, error) {
			live, err := conns.Get(ctx, id)
			return live.Connector, err
		})})
	payoutSvc.RegisterJobs(q)
	flow := checkout.New(checkout.Options{DB: db, Core: svc, Conns: conns, Routing: rt, Breaker: breaker, Logger: a.log, Disputes: disp, Subs: subs, Payouts: payoutSvc})
	svc.SetAttemptSyncer(flow) // expiring a payment asks the provider first
	if err := flow.Register(q); err != nil {
		return err
	}
	if err := disp.Register(q); err != nil {
		return err
	}
	rec := reconcile.New(reconcile.Options{DB: db, Core: svc, Conns: conns, Settings: set, Alerts: alerts, Loc: a.cfg.Location, Logger: a.log,
		KnownSubscription: knownSubscription(db), LinkRenewal: linkRenewal(conns, subs), Subscriptions: subs})
	if err := rec.Register(q); err != nil {
		return err
	}
	hk := housekeeping.New(housekeeping.Options{Core: svc,
		Domains: housekeeping.NewDomainChecker(housekeeping.DomainOptions{DB: db, Alerts: alerts, Logger: a.log,
			Fetch: housekeeping.DefaultFetch(a.cfg.Env == config.EnvDevelopment)}),
		Silence: housekeeping.NewSilenceDetector(housekeeping.SilenceOptions{DB: db, Alerts: alerts, Logger: a.log})})
	if err := hk.Register(q); err != nil {
		return err
	}
	if dbl != nil {
		if err := dbl.registerPurge(q, db, set); err != nil {
			return err
		}
	}
	if err := registerProviderEventPurge(q, db, set); err != nil {
		return err
	}
	var redactMessage func(string) string
	if dbl != nil && dbl.red != nil {
		redactMessage = dbl.red.Text // knows every account's secrets
	}
	api, err := httpapi.New(httpapi.Options{Core: svc, DB: db, Box: box, BaseURL: a.cfg.CustomerURL(), Log: a.log, Invoices: inv, Subscriptions: subs, PaymentLinks: links, Payouts: payoutSvc, Redact: redactMessage})
	if err != nil {
		return err
	}
	verifiers := a.verifiers
	if verifiers == nil {
		verifiers = conns.Verifiers()
	}
	hooks := inbound.New(inbound.Options{DB: db, Queue: q, Verifiers: verifiers, Logger: a.log,
		ClientIP: func(r *http.Request) string { return clientip.From(r, a.cfg.TrustedProxies) }})

	overlay, err := web.NewOverlay(web.Embedded(), filepath.Join(a.cfg.DataDir, "custom"))
	if err != nil {
		return err
	}
	// Themes: bundled ones plus <data>/themes/<name>/. Files are rechecked every
	// second, so adding or editing a theme needs no restart. A theme that cannot
	// be used is reported to ops once and its pages fall back to the embedded look.
	themes := web.NewThemes(web.Bundled(), filepath.Join(a.cfg.DataDir, "themes"))
	themes.CacheFor(time.Second)
	renderer, err := web.NewThemedRenderer(web.ThemedOptions{Base: web.Embedded(), CustomDir: filepath.Join(a.cfg.DataDir, "custom"),
		Themes: themes, Recheck: time.Second, Alert: func(what string, err error) {
			a.log.Warn("theme or catalog unusable", "name", what, "err", err)
			// Off the render path: sending may take a while.
			go func() {
				_ = alerts.Raise(context.Background(), notify.Alert{Severity: notify.SeverityWarning, Kind: "theme.unusable",
					Message: "Pages fall back to the standard look: " + err.Error(), Refs: map[string]string{"theme": what}})
			}()
		}})
	if err != nil {
		return err
	}
	csrf := sha256.Sum256(append([]byte("paymux-pay-csrf-key|"), a.cfg.MasterKey...))
	pay := paypage.New(paypage.Options{Core: svc, Flow: flow, Settings: set, Renderer: renderer,
		CSRFKey: csrf[:], Logger: a.log})
	linkPages := paylinks.NewHosted(paylinks.HostedOptions{Service: links, Core: svc, Renderer: renderer, Settings: set, CSRFKey: csrf[:],
		Secure: a.cfg.CustomerURL().Scheme == "https", Logger: a.log})

	a.registerMetrics(reg, db, dialect)
	registerBusinessMetrics(reg, db, a.log, time.Now)

	srv := server.New(server.Options{
		Logger:     a.log,
		Metrics:    reg,
		OnResponse: dbl.responseHook(),
		ClientIP:   func(r *http.Request) string { return clientip.From(r, a.cfg.TrustedProxies) },
		Checks: []server.Check{
			{Name: "db", Fn: db.Ping},
			{Name: "migrations", Fn: func(ctx context.Context) error {
				n, err := db.PendingMigrations(ctx, migrations.FS)
				if err != nil {
					return err
				}
				if n > 0 {
					return fmt.Errorf("%d migrations pending", n)
				}
				return nil
			}},
			{Name: "queue", Fn: func(context.Context) error {
				if !q.Running() {
					return errors.New("job queue not running")
				}
				return nil
			}},
			{Name: "scheduler", Fn: func(ctx context.Context) error {
				last, ok, err := db.ScheduleUpdatedAt(ctx, heartbeatKind)
				if err != nil {
					return err
				}
				if !schedulerHealthy(time.Now(), started, last, ok) {
					return errors.New("heartbeat schedule has not run recently")
				}
				return nil
			}},
		},
	})

	api.Register(srv)
	pay.Register(srv)
	pay.RegisterSim(srv) // base host only: custom domains serve no /sim
	srv.HandleFunc("POST /hooks/{provider_account_id}", hooks.ServeHTTP)

	// Custom domains serve the customer routes only; everything else stays on
	// the base host.
	customerRoutes := func() *http.ServeMux {
		m := http.NewServeMux()
		pay.Register(paypage.StdMux(m))
		m.HandleFunc("GET /i/{id}", inv.ServeHosted)
		m.HandleFunc("GET /l/{slug}", linkPages.Show)
		m.HandleFunc("POST /l/{slug}", linkPages.Submit)
		m.HandleFunc("GET /s/{id}", subs.ServeHosted)
		m.HandleFunc("POST /s/{id}", subs.SubmitBilling)
		m.Handle("GET /assets/", web.AssetHandler(overlay))
		m.Handle("GET /theme/", themes.Handler())
		return m
	}
	customerMux := customerRoutes()
	baseHost := a.cfg.BaseURL.Host
	router := &domains.Router{BaseHost: baseHost, Lookup: svc, Base: srv.Handler(), Customer: customerMux}
	tlsAllow := domains.TLSAllowOptions{BaseHost: baseHost, Lookup: svc}
	if co := a.cfg.CheckoutURL; co != nil {
		// PAYMUX_CHECKOUT_URL: the customer pages (and the simulator's) get a host
		// of their own that serves nothing else, and the base host redirects them.
		checkoutMux := customerRoutes()
		pay.RegisterSim(paypage.StdMux(checkoutMux))
		router.CheckoutHost, router.CheckoutOrigin, router.Checkout = co.Host, strings.TrimRight(co.String(), "/"), checkoutMux
		tlsAllow.ExtraHosts = []string{co.Host}
	}
	public := hostRouter(router, srv.Handler())
	internalMux := http.NewServeMux()
	internalMux.Handle("GET /internal/tls-allow", domains.NewTLSAllow(tlsAllow))
	internalMux.Handle("/", srv.InternalHandler())

	// Operator dashboard and the shared static assets.
	dashLogs, dashRedact := dbl.dashboardLogs()
	var updater dashboard.Updater
	a.mu.Lock()
	su := a.selfUpdate
	a.mu.Unlock()
	if su != nil {
		updater = selfupdate.New(selfupdate.Config{Repo: su.repo, Exe: su.exe, Current: func() string { return Version },
			Restart: a.requestRestart, Logger: a.log})
	}
	dash, err := dashboard.New(dashboard.Options{Updater: updater, LogDB: dashLogs, LogRedact: dashRedact, DB: db, Box: box, Renderer: renderer, Themes: themes, MasterKey: a.cfg.MasterKey,
		BaseURL: a.cfg.BaseURL, DevSkip2FA: a.cfg.DevSkip2FA, Logger: a.log,
		TrustedProxies: a.cfg.TrustedProxies, Core: svc, Settings: set, Outbound: out, Invoice: inv, Connectors: conns, Disputes: disp, Recon: rec, Jobs: q,
		Subscriptions: subs, PaymentLinks: links, Alerts: alertCenter, Location: a.cfg.Location,
		OIDC: a.oidcClient(), OIDCAllowedDomains: a.cfg.OIDCAllowedDomains, OIDCDefaultRole: a.cfg.OIDCDefaultRole})
	if err != nil {
		return err
	}
	inv.SetPages(renderer)
	subs.SetPages(renderer)
	subs.SetTemplates(overlay)
	srv.HandleFunc("GET /i/{id}", inv.ServeHosted)
	srv.HandleFunc("GET /l/{slug}", linkPages.Show)
	srv.HandleFunc("POST /l/{slug}", linkPages.Submit)
	srv.HandleFunc("GET /s/{id}", subs.ServeHosted)
	srv.HandleFunc("POST /s/{id}", subs.SubmitBilling)
	srv.HandleFunc("/dashboard/", dash.ServeHTTP)
	// First-run setup: open only while there are no users (the token is in the log).
	srv.HandleFunc("/setup", dash.SetupHandler().ServeHTTP)
	// The API reference, rendered from docs/openapi.yaml: base host only.
	apiDocs, err := apidocs.NewHandler(apidocs.Options{Spec: docs.OpenAPI, Renderer: renderer, Settings: set, Logger: a.log})
	if err != nil {
		return err
	}
	srv.HandleFunc("GET /docs", apiDocs.Page)
	srv.HandleFunc("GET /docs/openapi.yaml", apiDocs.Raw)
	srv.HandleFunc("GET /assets/", web.AssetHandler(overlay).ServeHTTP)
	srv.HandleFunc("GET /theme/", themes.Handler().ServeHTTP)
	// The bare host leads to first-run setup while there are no users, then
	// to the dashboard (which sends a signed-out browser to its login).
	srv.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if dash.SetupToken() != "" {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
	})
	// Browsers ask for /favicon.ico whatever the page says.
	srv.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets/favicon.svg", http.StatusMovedPermanently)
	})

	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen PAYMUX_LISTEN=%s: %w", a.cfg.Listen, err)
	}
	iln, err := net.Listen("tcp", a.cfg.InternalListen)
	if err != nil {
		ln.Close()
		return fmt.Errorf("listen PAYMUX_INTERNAL_LISTEN=%s: %w", a.cfg.InternalListen, err)
	}
	a.mu.Lock()
	a.addr, a.iaddr = ln.Addr().String(), iln.Addr().String()
	a.mu.Unlock()

	main, internal := newServer(public), newServer(internalMux)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	const workers = 3
	errc := make(chan error, workers)
	serve := func(s *http.Server, l net.Listener) {
		if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}
	a.startJSHooks(ctx)
	go serve(main, ln)
	go serve(internal, iln)
	go func() { errc <- q.Start(ctx) }()
	a.log.Info("paymux listening", "addr", a.Addr(), "internal_addr", a.InternalAddr())

	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errc:
		// a worker ended: stop the rest
		firstErr = unexpectedStop(ctx, firstErr)
		cancel()
		return a.stop(main, internal, errc, workers-1, firstErr)
	}
	return a.stop(main, internal, errc, workers, nil)
}

// unexpectedStop turns a worker's exit into the supervisor's result. A worker
// that returns nil after the shutdown signal cancelled ctx is a normal stop:
// without this check the select in Run could pick the worker's exit over
// ctx.Done() and report a clean shutdown as an error (exit code 1).
func unexpectedStop(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("paymux: component stopped unexpectedly")
}

// hostRouter sends requests by Host: the base host and custom domains go
// through router, but health probes and requests addressed by IP (load
// balancers, local tools) always reach the base handler.
func hostRouter(router, base http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || net.ParseIP(domains.Normalize(r.Host)) != nil {
			base.ServeHTTP(w, r)
			return
		}
		router.ServeHTTP(w, r)
	})
}

// stop drains the listeners gracefully and waits for pending workers.
func (a *App) stop(main, internal *http.Server, errc chan error, pending int, firstErr error) error {
	a.log.Info("paymux shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, s := range []*http.Server{main, internal} {
		if err := s.Shutdown(sctx); err != nil {
			_ = s.Close()
		}
	}
	for ; pending > 0; pending-- {
		if err := <-errc; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (a *App) registerMetrics(reg *metrics.Registry, db *store.DB, dialect string) {
	reg.Gauge("paymux_build_info", "Build information.", "version", "db").Set(1, Version, dialect)
	depth := reg.Gauge("paymux_jobs_depth", "Jobs by state.", "state")

	var mu sync.Mutex
	var oldest, heartbeat float64
	reg.GaugeFunc("paymux_jobs_oldest_pending_seconds", "Age of the oldest due pending job.", func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return oldest
	})
	reg.GaugeFunc("paymux_heartbeat_last_run_timestamp_seconds", "Unix time the system.heartbeat schedule last ran.", func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return heartbeat
	})
	reg.OnScrape(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stats, age, err := db.JobStats(ctx, time.Now())
		if err != nil {
			a.log.Warn("metrics: job stats", "err", err)
		} else {
			depth.Reset()
			for _, s := range []string{"pending", "running", "done", "dead"} {
				depth.Set(float64(stats[s]), s)
			}
			mu.Lock()
			oldest = age.Seconds()
			mu.Unlock()
		}
		at, ok, err := db.ScheduleUpdatedAt(ctx, heartbeatKind)
		if err != nil {
			a.log.Warn("metrics: heartbeat", "err", err)
			return
		}
		mu.Lock()
		heartbeat = 0
		if ok {
			heartbeat = float64(at.Unix())
		}
		mu.Unlock()
	})
}

// schedulerHealthy reports whether the heartbeat schedule has run recently.
// Schedules first fire one interval after registration, so the check only
// applies once the process has been up for heartbeatGrace.
func schedulerHealthy(now, started, last time.Time, haveLast bool) bool {
	if now.Sub(started) < heartbeatGrace {
		return true
	}
	return haveLast && now.Sub(last) <= heartbeatGrace
}

// oidcClient is the single sign-on client, or nil when it is not configured.
func (a *App) oidcClient() *oidc.Client {
	if !a.cfg.OIDCEnabled() {
		return nil
	}
	return oidc.New(oidc.Config{Issuer: a.cfg.OIDCIssuer, ClientID: a.cfg.OIDCClientID, ClientSecret: a.cfg.OIDCClientSecret.Reveal(),
		RedirectURL:   strings.TrimRight(a.cfg.BaseURL.String(), "/") + "/dashboard/login/oidc/callback",
		AllowInsecure: strings.HasPrefix(a.cfg.OIDCIssuer, "http://")}) // config allows http for localhost only
}
