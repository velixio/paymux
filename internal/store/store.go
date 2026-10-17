// Package store is the only package that talks to a database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/paymux/paymux/internal/store/dialect"
)

// ErrLocked means another process holds the SQLite data-dir lock.
var ErrLocked = errors.New("database is locked by another paymux process")

// DB is the store handle. SQLite uses one writer plus a read pool;
// Postgres uses a single pool for both.
type DB struct {
	writer, reader *sql.DB
	d              dialect.Dialect
	unlock         func()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Querier runs read-only queries. Placeholders are always '?'.
type Querier struct {
	e execer
	d dialect.Dialect
}

// Tx is a write transaction. Placeholders are always '?'.
type Tx struct {
	tx *sql.Tx
	d  dialect.Dialect
}

func (q Querier) ExecContext(ctx context.Context, s string, a ...any) (sql.Result, error) {
	return q.e.ExecContext(ctx, q.d.Rebind(s), a...)
}
func (q Querier) QueryContext(ctx context.Context, s string, a ...any) (*sql.Rows, error) {
	return q.e.QueryContext(ctx, q.d.Rebind(s), a...)
}
func (q Querier) QueryRowContext(ctx context.Context, s string, a ...any) *sql.Row {
	return q.e.QueryRowContext(ctx, q.d.Rebind(s), a...)
}

func (t Tx) ExecContext(ctx context.Context, s string, a ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, t.d.Rebind(s), a...)
}
func (t Tx) QueryContext(ctx context.Context, s string, a ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.d.Rebind(s), a...)
}
func (t Tx) QueryRowContext(ctx context.Context, s string, a ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.Rebind(s), a...)
}

// Dialect returns the dialect of the underlying database.
func (t Tx) Dialect() dialect.Dialect { return t.d }

// Open selects the database from databaseURL (empty means
// sqlite://<dataDir>/paymux.db) and connects.
func Open(ctx context.Context, databaseURL, dataDir string, logger *slog.Logger) (*DB, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if databaseURL == "" {
		databaseURL = "sqlite://" + strings.TrimSuffix(dataDir, "/") + "/paymux.db"
	}
	redacted := redactURL(databaseURL)
	var (
		db  *DB
		err error
	)
	switch {
	case strings.HasPrefix(databaseURL, "sqlite://"):
		db, err = openSQLite(ctx, strings.TrimPrefix(databaseURL, "sqlite://"))
	case strings.HasPrefix(databaseURL, "postgres://"), strings.HasPrefix(databaseURL, "postgresql://"):
		db, err = openPostgres(ctx, databaseURL)
	default:
		return nil, fmt.Errorf("open PAYMUX_DATABASE_URL=%s: unsupported scheme (want sqlite:// or postgres://)", redacted)
	}
	if err != nil {
		return nil, wrapOpenErr(redacted, databaseURL, err)
	}
	logger.Info("database opened", "dialect", db.d.Name(), "url", redacted)
	return db, nil
}

// wrapOpenErr names PAYMUX_DATABASE_URL and the redacted URL, and scrubs the
// password from the underlying error text. ErrLocked stays matchable.
func wrapOpenErr(redacted, raw string, err error) error {
	msg := err.Error()
	if u, perr := url.Parse(raw); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "xxxxx")
			msg = strings.ReplaceAll(msg, url.QueryEscape(pw), "xxxxx")
		}
	}
	if errors.Is(err, ErrLocked) {
		return fmt.Errorf("open PAYMUX_DATABASE_URL=%s: %s: %w", redacted, msg, ErrLocked)
	}
	return fmt.Errorf("open PAYMUX_DATABASE_URL=%s: %s", redacted, msg)
}

// redactURL replaces the password in a URL with "xxxxx".
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		if err != nil && strings.Contains(raw, "@") {
			return "[unparseable url]"
		}
		return raw
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

// Dialect returns the database dialect.
func (db *DB) Dialect() dialect.Dialect { return db.d }

// Write runs fn in a transaction: commit on nil, rollback on error or panic
// (the panic is re-raised).
func (db *DB) Write(ctx context.Context, fn func(tx Tx) error) (err error) {
	opts := &sql.TxOptions{}
	if db.d.Name() == "postgres" {
		opts.Isolation = sql.LevelReadCommitted
	}
	tx, err := db.writer.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()
	if err := fn(Tx{tx: tx, d: db.d}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Read runs fn against the read pool.
func (db *DB) Read(ctx context.Context, fn func(q Querier) error) error {
	return fn(Querier{e: db.reader, d: db.d})
}

// Ping probes the write path.
func (db *DB) Ping(ctx context.Context) error {
	return db.Write(ctx, func(tx Tx) error {
		var one int
		return tx.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	})
}

// Close closes the pools and releases the SQLite lock.
func (db *DB) Close() error {
	err := db.writer.Close()
	if db.reader != db.writer {
		if e := db.reader.Close(); err == nil {
			err = e
		}
	}
	if db.unlock != nil {
		db.unlock()
	}
	return err
}
