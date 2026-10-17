// Package paymux is the root of the Paymux payments hub.
package paymux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/metrics"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
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
	cfg config.Config
	log *slog.Logger

	verifiers inbound.VerifierFunc

	mu          sync.Mutex
	addr, iaddr string
}

// WithVerifiers sets how inbound webhooks find the Verifier of a provider
// account. The default knows no accounts (every /hooks/{id} is a 404); the
// connector lane supplies the real lookup.
func WithVerifiers(f inbound.VerifierFunc) Option { return func(a *App) { a.verifiers = f } }

// errNotWired fails provider-event jobs until the connector lane supplies
// their processing, so they retry instead of being silently dropped.
var errNotWired = errors.New("provider event processing is not wired yet")

// Option customises an App.
type Option func(*App)

// WithLogger sets the logger. The default writes redacted JSON to stderr.
func WithLogger(l *slog.Logger) Option { return func(a *App) { a.log = l } }

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
	if a.verifiers == nil {
		a.verifiers = func(context.Context, string) (inbound.Verifier, error) { return nil, inbound.ErrUnknownAccount }
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

	started := time.Now()
	q := jobs.New(db, jobs.Options{Logger: a.log})
	q.Register(heartbeatKind, func(context.Context, json.RawMessage) error { return nil })
	if err := q.AddSchedule(jobs.Schedule{Name: heartbeatKind, Kind: heartbeatKind, Every: heartbeatEvery}); err != nil {
		return err
	}

	box, err := secrets.NewBox(a.cfg.MasterKey)
	if err != nil {
		return err
	}
	// The outbound service is the events.Emitter other lanes will be given.
	out := outbound.New(outbound.Options{DB: db, Queue: q, Box: box,
		AllowPrivate: a.cfg.AllowPrivateWebhooks, Logger: a.log})
	out.Register()
	// TODO(spec): the connector lane replaces these with real processing
	// (ParseWebhook, Fetch*, apply transition) for the §6.4 jobs.
	for _, kind := range []string{inbound.JobProcess, inbound.JobFetchOnly} {
		q.Register(kind, func(context.Context, json.RawMessage) error { return errNotWired })
	}
	hooks := inbound.New(inbound.Options{DB: db, Queue: q, Verifiers: a.verifiers, Logger: a.log})

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

	srv.HandleFunc("POST /hooks/{provider_account_id}", hooks.ServeHTTP)

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

	main := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: readHeaderTimeout}
	internal := &http.Server{Handler: srv.InternalHandler(), ReadHeaderTimeout: readHeaderTimeout}

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
