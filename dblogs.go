package paymux

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/url"
	"time"

	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/redact"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
)

// dbLogs is the error log kept in the database: a redactor that knows the
// secrets in use, the recorder that writes rows, and the database they go to.
type dbLogs struct {
	rec *dblog.Recorder
	red *redact.Redactor
	db  *store.DB // the logs database; main is the application database
	// shared is true when the logs table is in the main database (PostgreSQL).
	shared bool
}

// openDBLogs prepares the database log and tees a.log into it. A log that
// cannot be opened is not worth failing over: Paymux runs without it and says
// so on stdout. The returned value is nil in that case; its methods are not
// called on nil.
func (a *App) openDBLogs(ctx context.Context, main *store.DB) *dbLogs {
	red := redact.New()
	a.registerConfigSecrets(red)
	ldb, err := main.OpenLogs(ctx, migrations.FS)
	if err != nil {
		a.log.Error("the error log in the database is unavailable; continuing without it", "err", err.Error())
		return nil
	}
	level := slog.LevelWarn
	// A level below warn is kept only when the operator asked for it by name.
	if a.cfg.DBLogLevel != slog.LevelWarn && (a.cfg.DBLogLevel > slog.LevelWarn || a.cfg.DBLogLevelExplicit) {
		level = a.cfg.DBLogLevel
	}
	rec := dblog.New(dblog.Options{DB: ldb, Redactor: red, Level: level, Stdout: a.log})
	rec.Start(ctx)
	l := &dbLogs{rec: rec, red: red, db: ldb, shared: ldb == main}
	a.mu.Lock()
	a.logDB, a.logRec = ldb, rec
	a.mu.Unlock()
	a.log = slog.New(rec.Handler(a.log.Handler()))
	return l
}

// close writes what is queued and releases the logs database.
func (l *dbLogs) close() {
	if l == nil {
		return
	}
	l.rec.Close()
	if !l.shared {
		_ = l.db.Close()
	}
}

// logStore returns the logs database and recorder (nil before Start has opened them).
func (a *App) logStore() (*store.DB, *dblog.Recorder) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logDB, a.logRec
}

// registerConfigSecrets tells the redactor the secrets that come from the
// environment: the master key in every form it may appear in, the SMTP and
// OIDC secrets, the ops webhook URL (its path is a token), the database
// password and the OTLP headers.
func (a *App) registerConfigSecrets(red *redact.Redactor) {
	var vals []string
	if k := a.cfg.MasterKey; len(k) > 0 {
		vals = append(vals, base64.StdEncoding.EncodeToString(k), base64.RawStdEncoding.EncodeToString(k),
			base64.URLEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k), hex.EncodeToString(k), string(k))
	}
	vals = append(vals, a.cfg.SMTPPassword.Reveal(), a.cfg.OIDCClientSecret.Reveal(), a.cfg.OpsWebhookURL)
	if u, err := url.Parse(a.cfg.OpsWebhookURL); err == nil && u.Path != "" && u.Path != "/" {
		vals = append(vals, u.Path)
	}
	if u, err := url.Parse(a.cfg.DatabaseURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			vals = append(vals, pw)
		}
	}
	for _, h := range a.cfg.OTLPLogsHeaders {
		vals = append(vals, h.Reveal())
	}
	red.SetKnown("config", vals...)
}

// logPurgeKind is the scheduled job that trims the log and the audit log.
const logPurgeKind = "logs.purge"

// registerPurge schedules the hourly purge: log rows past their retention
// (errors and warnings have their own), the row cap, and the audit log's
// retention. The job queue's lease makes sure one instance runs it at a time.
func (l *dbLogs) registerPurge(q *jobs.Queue, main *store.DB, set *settings.Service) error {
	p := dblog.NewPurger(dblog.PurgeOptions{Logs: l.db, Main: main,
		Retention: func(ctx context.Context, key string) (int, error) {
			v, err := set.ResolveOrg(ctx, store.DefaultOrgID, key) // TODO(spec): one org today; retention is not per org
			if err != nil {
				return 0, err
			}
			var n int
			return n, v.Decode(&n)
		}})
	q.Register(logPurgeKind, func(ctx context.Context, _ json.RawMessage) error {
		_, err := p.Run(ctx)
		return err
	})
	return q.AddSchedule(jobs.Schedule{Name: logPurgeKind, Kind: logPurgeKind, Every: time.Hour})
}

// responseHook records finished requests that are worth keeping.
func (l *dbLogs) responseHook() server.ResponseHook {
	if l == nil {
		return nil
	}
	return func(ctx context.Context, method, route string, status int, d time.Duration) {
		l.rec.HTTP(ctx, dblog.HTTPEvent{Method: method, Route: route, Status: status, Duration: d})
	}
}
