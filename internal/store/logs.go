package store

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logsMigrationFile is the migration whose statements create the logs table.
// PostgreSQL runs it as part of the main migrations; SQLite runs the same file
// against its own database, <data>/logs.db.
const logsMigrationFile = "0021_logs.sql"

// Path returns the SQLite database file ("" on PostgreSQL).
func (db *DB) Path() string { return db.path }

// OpenLogs returns the database that holds the logs table. On PostgreSQL that
// is db itself (the caller must not close it twice). On SQLite it is a second
// database file next to the main one, opened and migrated here, so log writes
// never take the main database's write lock. The file can be deleted at any
// time: a damaged one is set aside and recreated.
func (db *DB) OpenLogs(ctx context.Context, migrations fs.FS) (*DB, error) {
	if db.d.Name() != "sqlite" {
		return db, nil
	}
	path := filepath.Join(filepath.Dir(db.path), "logs.db")
	l, err := openLogsFile(ctx, path, migrations)
	if err == nil {
		return l, nil
	}
	aside := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
	if rerr := os.Rename(path, aside); rerr != nil {
		return nil, err
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	return openLogsFile(ctx, path, migrations)
}

func openLogsFile(ctx context.Context, path string, migrations fs.FS) (*DB, error) {
	body, err := fs.ReadFile(migrations, "sqlite/"+logsMigrationFile)
	if err != nil {
		return nil, fmt.Errorf("logs migration: %w", err)
	}
	l, err := openSQLiteFile(ctx, path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) { l.Close(); return nil, err }
	applied, err := l.appliedVersions(ctx)
	if err != nil {
		return fail(err)
	}
	const version = 21
	if !applied[version] {
		if err := l.applyOne(ctx, migration{version: version, name: logsMigrationFile, sql: string(body)}); err != nil {
			return fail(fmt.Errorf("logs migration: %w", err))
		}
	}
	return l, nil
}

// LogRow is a row of logs. Every text field is already redacted.
type LogRow struct {
	ID                                string
	At                                time.Time
	Level, Kind, Component, Msg       string
	RequestID, JobID, OrgID, AppID    string
	PaymentID, AttemptID, RefundID    string
	SubscriptionID, ProviderAccountID string
	Connector                         string
	IDs, Attrs, Request, Response     string // JSON documents
	Status                            int
	DurationMS                        int64
}

const logColumns = `id, at, level, kind, component, msg, request_id, job_id, org_id, app_id, payment_id, attempt_id, refund_id,
	subscription_id, provider_account_id, connector, ids, attrs, request, response, status, duration_ms`

// InsertLogs writes rows (in chunks, in the caller's transaction).
func InsertLogs(ctx context.Context, tx Tx, rows []LogRow) error {
	const chunk = 40
	for len(rows) > 0 {
		n := min(chunk, len(rows))
		var sb strings.Builder
		var args []any
		sb.WriteString("INSERT INTO logs (" + logColumns + ") VALUES ")
		for i, r := range rows[:n] {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, r.ID, ms(r.At), r.Level, r.Kind, r.Component, r.Msg,
				nullIfEmpty(r.RequestID), nullIfEmpty(r.JobID), nullIfEmpty(r.OrgID), nullIfEmpty(r.AppID),
				nullIfEmpty(r.PaymentID), nullIfEmpty(r.AttemptID), nullIfEmpty(r.RefundID), nullIfEmpty(r.SubscriptionID),
				nullIfEmpty(r.ProviderAccountID), nullIfEmpty(r.Connector), nullIfEmpty(r.IDs), nullIfEmpty(r.Attrs),
				nullIfEmpty(r.Request), nullIfEmpty(r.Response), nullIfZero(int64(r.Status)), nullIfZero(r.DurationMS))
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return err
		}
		rows = rows[n:]
	}
	return nil
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// LogFilter selects log rows. Empty fields do not restrict.
type LogFilter struct {
	From, To                     time.Time // At >= From, At < To
	Level, Kind                  string
	Connector, ProviderAccountID string
	RequestID, PaymentID         string
	AnyID                        string // matches any id column, or a value in the ids JSON
	Text                         string // case-insensitive substring of the message, attributes, ids and bodies
	BeforeAt                     time.Time
	BeforeID                     string // keyset cursor: rows older than (BeforeAt, BeforeID)
	Limit                        int    // default 100
	WithBodies                   bool   // include request and response (large)
	// OrgID scopes the rows to one organisation (rows of other orgs are
	// excluded). Rows that carry no org (process-wide errors) are included
	// only with IncludeUnscoped.
	OrgID           string
	IncludeUnscoped bool
}

// escapeLike escapes LIKE wildcards in s for a pattern with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func (f LogFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(c string, a ...any) { conds = append(conds, c); args = append(args, a...) }
	if f.OrgID != "" {
		if f.IncludeUnscoped {
			add("(org_id = ? OR org_id IS NULL OR org_id = '')", f.OrgID)
		} else {
			add("org_id = ?", f.OrgID)
		}
	}
	if !f.From.IsZero() {
		add("at >= ?", ms(f.From))
	}
	if !f.To.IsZero() {
		add("at < ?", ms(f.To))
	}
	if f.Level != "" {
		add("level = ?", f.Level)
	}
	if f.Kind != "" {
		add("kind = ?", f.Kind)
	}
	if f.Connector != "" {
		add("connector = ?", f.Connector)
	}
	if f.ProviderAccountID != "" {
		add("provider_account_id = ?", f.ProviderAccountID)
	}
	if f.RequestID != "" {
		add("request_id = ?", f.RequestID)
	}
	if f.PaymentID != "" {
		add("payment_id = ?", f.PaymentID)
	}
	if f.AnyID != "" {
		add(`(request_id = ? OR job_id = ? OR org_id = ? OR app_id = ? OR payment_id = ? OR attempt_id = ? OR refund_id = ?
			OR subscription_id = ? OR provider_account_id = ? OR ids LIKE ? ESCAPE '\')`,
			f.AnyID, f.AnyID, f.AnyID, f.AnyID, f.AnyID, f.AnyID, f.AnyID, f.AnyID, f.AnyID, `%"`+escapeLike(f.AnyID)+`"%`)
	}
	if f.Text != "" {
		p := "%" + escapeLike(strings.ToLower(f.Text)) + "%"
		add(`(LOWER(msg) LIKE ? ESCAPE '\' OR LOWER(component) LIKE ? ESCAPE '\' OR LOWER(COALESCE(attrs,'')) LIKE ? ESCAPE '\'
			OR LOWER(COALESCE(ids,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(request,'')) LIKE ? ESCAPE '\' OR LOWER(COALESCE(response,'')) LIKE ? ESCAPE '\')`,
			p, p, p, p, p, p)
	}
	if !f.BeforeAt.IsZero() {
		add("(at < ? OR (at = ? AND id < ?))", ms(f.BeforeAt), ms(f.BeforeAt), f.BeforeID)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func scanLog(rows interface{ Scan(...any) error }, r *LogRow) error {
	var at, status, dur int64
	if err := rows.Scan(&r.ID, &at, &r.Level, &r.Kind, &r.Component, &r.Msg, &r.RequestID, &r.JobID, &r.OrgID, &r.AppID,
		&r.PaymentID, &r.AttemptID, &r.RefundID, &r.SubscriptionID, &r.ProviderAccountID, &r.Connector, &r.IDs, &r.Attrs,
		&r.Request, &r.Response, &status, &dur); err != nil {
		return err
	}
	r.At, r.Status, r.DurationMS = fromMS(at), int(status), dur
	return nil
}

func logSelect(bodies bool) string {
	req, resp := "''", "''"
	if bodies {
		req, resp = "COALESCE(request,'')", "COALESCE(response,'')"
	}
	return `SELECT id, at, level, kind, component, msg, COALESCE(request_id,''), COALESCE(job_id,''), COALESCE(org_id,''),
		COALESCE(app_id,''), COALESCE(payment_id,''), COALESCE(attempt_id,''), COALESCE(refund_id,''), COALESCE(subscription_id,''),
		COALESCE(provider_account_id,''), COALESCE(connector,''), COALESCE(ids,''), COALESCE(attrs,''), ` + req + `, ` + resp + `,
		COALESCE(status,0), COALESCE(duration_ms,0) FROM logs`
}

// SearchLogs returns the newest rows first.
func SearchLogs(ctx context.Context, q Queryer, f LogFilter) ([]LogRow, error) {
	where, args := f.where()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, logSelect(f.WithBodies)+where+" ORDER BY at DESC, id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogRow
	for rows.Next() {
		var r LogRow
		if err := scanLog(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLog returns one row with its bodies, or ErrNotFound.
func GetLog(ctx context.Context, q Queryer, id string) (LogRow, error) {
	var r LogRow
	err := scanLog(q.QueryRowContext(ctx, logSelect(true)+" WHERE id = ?", id), &r)
	return r, notFound(err)
}

// CountLogs returns the number of rows.
func CountLogs(ctx context.Context, q Queryer) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM logs").Scan(&n)
	return n, err
}

// PurgeLogs deletes up to limit rows of one level older than before and
// returns how many it deleted.
func PurgeLogs(ctx context.Context, tx Tx, level string, before time.Time, limit int) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE id IN (
		SELECT id FROM logs WHERE level = ? AND at < ? ORDER BY at LIMIT ?)`, level, ms(before), limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// TrimLogs deletes the oldest rows beyond the newest keep, at most limit at a
// time, and returns how many it deleted.
func TrimLogs(ctx context.Context, tx Tx, keep, limit int) (int, error) {
	total, err := CountLogs(ctx, tx)
	if err != nil || total <= keep {
		return 0, err
	}
	n := min(total-keep, limit)
	res, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE id IN (SELECT id FROM logs ORDER BY at ASC, id ASC LIMIT ?)`, n)
	if err != nil {
		return 0, err
	}
	d, err := res.RowsAffected()
	return int(d), err
}
