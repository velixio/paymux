// Package paymux is the root of the Paymux payments hub.
package paymux

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
	"sync"
	"time"

	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/dashboard"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/metrics"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/paypage"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/migrations"
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
)

// App is a Paymux instance.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	emitter events.Emitter // nil means the outbound service

	verifiers inbound.VerifierFunc
	connHTTP  func(store.ProviderAccount, connector.Info, []string) *http.Client

	mu          sync.Mutex
	addr, iaddr string
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

// Start opens the store, migrates, starts the job queue and both listeners,
// and blocks until ctx is cancelled (returning nil after a graceful stop) or
// something fails.
func (a *App) Start(ctx context.Context) error {
	db, err := store.Open(ctx, a.cfg.DatabaseURL, a.cfg.DataDir, a.log)
	if err != nil {
		return err
	}
	defer db.Close()

	dialect := db.Dialect().Name()
	a.log.Info("paymux starting", "version", Version, "dialect", dialect,
		"database", a.cfg.RedactedDatabaseURL(), "env", string(a.cfg.Env))
	if a.cfg.DevSkip2FA {
		a.log.Warn("2FA is disabled (development)")
	}

	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return err
	}

	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	box, err := secrets.NewBox(a.cfg.MasterKey)
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
	breaker := routing.NewBreaker(nil)
	rt := checkout.NewRouting(breaker)
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: a.cfg.BaseURL, Logger: a.log, HTTPClient: a.connHTTP})
	// GST invoicing: documents are issued by hooks inside the transaction in
	// which a payment or refund succeeds.
	inv := invoice.New(invoice.Options{DB: db, Settings: set, Jobs: q, Emitter: emitter, Mailer: notify.New(a.cfg),
		BaseURL: a.cfg.BaseURL.String()})
	inv.RegisterJobs(q)
	svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: q, Emitter: emitter, Router: rt,
		OnPaymentSucceeded: inv.OnPaymentSucceeded, OnRefundSucceeded: inv.OnRefundSucceeded})
	flow := checkout.New(checkout.Options{DB: db, Core: svc, Conns: conns, Routing: rt, Breaker: breaker, Logger: a.log})
	if err := flow.Register(q); err != nil {
		return err
	}
	api, err := httpapi.New(httpapi.Options{Core: svc, DB: db, Box: box, BaseURL: a.cfg.BaseURL, Log: a.log, Invoices: inv})
	if err != nil {
		return err
	}
	verifiers := a.verifiers
	if verifiers == nil {
		verifiers = conns.Verifiers()
	}
	hooks := inbound.New(inbound.Options{DB: db, Queue: q, Verifiers: verifiers, Logger: a.log})

	overlay, err := web.NewOverlay(web.Embedded(), filepath.Join(a.cfg.DataDir, "custom"))
	if err != nil {
		return err
	}
	renderer, err := web.NewRenderer(overlay)
	if err != nil {
		return err
	}
	csrf := sha256.Sum256(append([]byte("paymux-pay-csrf-key|"), a.cfg.MasterKey...))
	pay := paypage.New(paypage.Options{Core: svc, Flow: flow, Settings: set, Renderer: renderer,
		CSRFKey: csrf[:], Logger: a.log})

	reg := metrics.NewRegistry()
	a.registerMetrics(reg, db, dialect)

	srv := server.New(server.Options{
		Logger:  a.log,
		Metrics: reg,
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
	srv.HandleFunc("POST /hooks/{provider_account_id}", hooks.ServeHTTP)

	// Custom domains serve the customer routes only; everything else stays on
	// the base host.
	customerMux := http.NewServeMux()
	pay.Register(paypage.StdMux(customerMux))
	customerMux.HandleFunc("GET /i/{id}", inv.ServeHosted)
	customerMux.Handle("GET /assets/", web.AssetHandler(overlay))
	baseHost := a.cfg.BaseURL.Host
	public := hostRouter(&domains.Router{BaseHost: baseHost, Lookup: svc, Base: srv.Handler(), Customer: customerMux}, srv.Handler())
	internalMux := http.NewServeMux()
	internalMux.Handle("GET /internal/tls-allow", domains.TLSAllowHandler(baseHost, svc))
	internalMux.Handle("/", srv.InternalHandler())

	// Operator dashboard and the shared static assets.
	dash, err := dashboard.New(dashboard.Options{DB: db, Box: box, Renderer: renderer, MasterKey: a.cfg.MasterKey,
		BaseURL: a.cfg.BaseURL, DevSkip2FA: a.cfg.DevSkip2FA, Logger: a.log})
	if err != nil {
		return err
	}
	inv.SetPages(renderer)
	srv.HandleFunc("GET /i/{id}", inv.ServeHosted)
	srv.HandleFunc("/dashboard/", dash.ServeHTTP)
	srv.HandleFunc("GET /assets/", web.AssetHandler(overlay).ServeHTTP)

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

	main := &http.Server{Handler: public, ReadHeaderTimeout: readHeaderTimeout}
	internal := &http.Server{Handler: internalMux, ReadHeaderTimeout: readHeaderTimeout}

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
	go serve(main, ln)
	go serve(internal, iln)
	go func() { errc <- q.Start(ctx) }()
	a.log.Info("paymux listening", "addr", a.Addr(), "internal_addr", a.InternalAddr())

	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errc:
		// a worker ended on its own: stop the rest
		if firstErr == nil {
			firstErr = errors.New("paymux: component stopped unexpectedly")
		}
		cancel()
		return a.stop(main, internal, errc, workers-1, firstErr)
	}
	return a.stop(main, internal, errc, workers, nil)
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
