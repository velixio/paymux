package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- apps ----

// ListApps returns the org's apps, oldest first.
func ListApps(ctx context.Context, q Queryer, orgID string) ([]App, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+appCols+" FROM apps WHERE org_id = ? ORDER BY created_at, id", orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func updateApp(ctx context.Context, tx Tx, id string, now time.Time, set string, arg any) error {
	res, err := tx.ExecContext(ctx, "UPDATE apps SET "+set+", updated_at = ? WHERE id = ?", arg, ms(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppBranding replaces the app's branding JSON document.
func SetAppBranding(ctx context.Context, tx Tx, id, brandingJSON string, now time.Time) error {
	return updateApp(ctx, tx, id, now, "branding = ?", brandingJSON)
}

// SetAppReturnURL sets (or, when empty, clears) the app's default return URL.
func SetAppReturnURL(ctx context.Context, tx Tx, id, url string, now time.Time) error {
	return updateApp(ctx, tx, id, now, "default_return_url = ?", nullIfEmpty(url))
}

// GetAppBranding returns the app's branding JSON document.
func GetAppBranding(ctx context.Context, q Queryer, id string) (string, error) {
	var b string
	err := q.QueryRowContext(ctx, "SELECT branding FROM apps WHERE id = ?", id).Scan(&b)
	return b, notFound(err)
}

// ---- API keys ----

// ListAPIKeys returns an app's keys, oldest first (revoked ones included).
func ListAPIKeys(ctx context.Context, q Queryer, appID string) ([]APIKey, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, org_id, app_id, mode, prefix, sha256_hash, last_used_at, revoked_at, created_at
		FROM api_keys WHERE app_id = ? ORDER BY created_at, id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var used, revoked sql.NullInt64
		var created int64
		if err := rows.Scan(&k.ID, &k.OrgID, &k.AppID, &k.Mode, &k.Prefix, &k.Hash, &used, &revoked, &created); err != nil {
			return nil, err
		}
		k.LastUsedAt, k.RevokedAt, k.CreatedAt = fromNullMS(used), fromNullMS(revoked), fromMS(created)
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes the app's key. It reports false when the key does not
// belong to the app or is already revoked.
func RevokeAPIKey(ctx context.Context, tx Tx, appID, keyID string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND app_id = ? AND revoked_at IS NULL`,
		ms(now), keyID, appID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- blobs ----

// Blob is a row of blobs holding its bytes in the database.
type Blob struct {
	ID, OrgID, Kind, ContentType, SHA256 string
	Size                                 int64
	Data                                 []byte
	CreatedAt                            time.Time
}

// InsertBlob stores b. Size and SHA256 are always computed from Data.
func InsertBlob(ctx context.Context, tx Tx, b Blob) error {
	sum := sha256.Sum256(b.Data)
	b.SHA256 = fmt.Sprintf("%x", sum)
	_, err := tx.ExecContext(ctx, `INSERT INTO blobs (id, org_id, kind, content_type, size, sha256, storage, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'db', ?, ?)`, b.ID, b.OrgID, b.Kind, b.ContentType, len(b.Data), b.SHA256, b.Data, ms(b.CreatedAt))
	return err
}

// GetBlob loads a blob, or returns ErrNotFound.
func GetBlob(ctx context.Context, q Queryer, id string) (Blob, error) {
	var b Blob
	var created int64
	err := q.QueryRowContext(ctx, `SELECT id, org_id, kind, content_type, size, sha256, data, created_at FROM blobs WHERE id = ?`, id).
		Scan(&b.ID, &b.OrgID, &b.Kind, &b.ContentType, &b.Size, &b.SHA256, &b.Data, &created)
	if err != nil {
		return Blob{}, notFound(err)
	}
	b.CreatedAt = fromMS(created)
	return b, nil
}

// ---- provider accounts ----

// ProviderAccountInfo is a provider account without its encrypted config.
type ProviderAccountInfo struct {
	ID, OrgID, Mode, Connector, Name, Status string
	Currencies, Purposes                     []string
}

// InsertProviderAccountInfo inserts an account with its accepted currencies
// and enabled purposes (no config).
func InsertProviderAccountInfo(ctx context.Context, tx Tx, pa ProviderAccountInfo, now time.Time) error {
	cur, _ := json.Marshal(nonNilSlice(pa.Currencies))
	pur, _ := json.Marshal(nonNilSlice(pa.Purposes))
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_accounts
		(id, org_id, mode, connector, name, status, accepted_currencies, enabled_purposes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, pa.ID, pa.OrgID, pa.Mode, pa.Connector, pa.Name, pa.Status,
		string(cur), string(pur), ms(now), ms(now))
	return err
}

// ListProviderAccounts lists the org's provider accounts, optionally of one mode.
func ListProviderAccounts(ctx context.Context, q Queryer, orgID, mode string) ([]ProviderAccountInfo, error) {
	query := `SELECT id, org_id, mode, connector, name, status, accepted_currencies, enabled_purposes
		FROM provider_accounts WHERE org_id = ?`
	args := []any{orgID}
	if mode != "" {
		query += " AND mode = ?"
		args = append(args, mode)
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY created_at, id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderAccountInfo
	for rows.Next() {
		var pa ProviderAccountInfo
		var cur, pur string
		if err := rows.Scan(&pa.ID, &pa.OrgID, &pa.Mode, &pa.Connector, &pa.Name, &pa.Status, &cur, &pur); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cur), &pa.Currencies); err != nil {
			return nil, fmt.Errorf("provider account %s currencies: %w", pa.ID, err)
		}
		if err := json.Unmarshal([]byte(pur), &pa.Purposes); err != nil {
			return nil, fmt.Errorf("provider account %s purposes: %w", pa.ID, err)
		}
		out = append(out, pa)
	}
	return out, rows.Err()
}

// ListAppProviderAccountIDs returns the ids of the accounts an app may use.
func ListAppProviderAccountIDs(ctx context.Context, q Queryer, appID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT provider_account_id FROM app_provider_accounts WHERE app_id = ? ORDER BY provider_account_id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetAppProviderAccounts replaces the set of accounts an app may use.
func SetAppProviderAccounts(ctx context.Context, tx Tx, appID string, accountIDs []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_provider_accounts WHERE app_id = ?`, appID); err != nil {
		return err
	}
	for _, id := range accountIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_provider_accounts (app_id, provider_account_id) VALUES (?, ?)
			ON CONFLICT (app_id, provider_account_id) DO NOTHING`, appID, id); err != nil {
			return err
		}
	}
	return nil
}

// ---- payments ----

// PaymentFilter selects payments for the dashboard list. Empty fields match
// anything. From is inclusive, To exclusive. Search matches the id prefix,
// part of the order_ref or part of the customer's email (case-insensitive,
// wildcards literal).
type PaymentFilter struct {
	OrgID                       string // required: an empty org matches nothing
	AppID, Mode, Status, Search string
	From, To                    time.Time
}

// likeEscape escapes LIKE wildcards in s.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(s))
}

// SearchPayments returns matching payments newest first. after is an exclusive
// id cursor.
func SearchPayments(ctx context.Context, q Queryer, f PaymentFilter, afterID string, limit int) ([]Payment, error) {
	query := "SELECT " + paymentCols + " FROM payments WHERE org_id = ?"
	args := []any{f.OrgID}
	add := func(cond string, a ...any) {
		query += " AND " + cond
		args = append(args, a...)
	}
	if f.AppID != "" {
		add("app_id = ?", f.AppID)
	}
	if f.Mode != "" {
		add("mode = ?", f.Mode)
	}
	if f.Status != "" {
		add("status = ?", f.Status)
	}
	if !f.From.IsZero() {
		add("created_at >= ?", ms(f.From))
	}
	if !f.To.IsZero() {
		add("created_at < ?", ms(f.To))
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		e := likeEscape(s)
		add(`(LOWER(id) LIKE ? ESCAPE '\' OR LOWER(COALESCE(order_ref,'')) LIKE ? ESCAPE '\'
			OR customer_id IN (SELECT id FROM customers WHERE LOWER(COALESCE(email,'')) LIKE ? ESCAPE '\'))`,
			e+"%", "%"+e+"%", "%"+e+"%")
	}
	if afterID != "" {
		add("id < ?", afterID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- ledger totals ----

// LedgerTotal is the money moved for one app and currency: Gross is the sum
// of charge entries, Refunds the (positive) sum of refund entries.
type LedgerTotal struct {
	AppID, Currency string
	Gross, Refunds  int64
}

// LedgerTotals sums charge and refund ledger entries of one org and mode since
// the given time, per app and currency.
func LedgerTotals(ctx context.Context, q Queryer, orgID, mode string, since time.Time) ([]LedgerTotal, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.app_id, l.currency,
			COALESCE(SUM(CASE WHEN l.kind = 'charge' THEN l.amount ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN l.kind = 'refund' THEN -l.amount ELSE 0 END), 0)
		FROM ledger_entries l JOIN payments p ON p.id = l.payment_id
		WHERE l.org_id = ? AND p.org_id = ? AND l.mode = ? AND l.occurred_at >= ? AND l.kind IN ('charge','refund')
		GROUP BY p.app_id, l.currency ORDER BY p.app_id, l.currency`, orgID, orgID, mode, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerTotal
	for rows.Next() {
		var t LedgerTotal
		if err := rows.Scan(&t.AppID, &t.Currency, &t.Gross, &t.Refunds); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- audit ----

// AuditFilter selects audit rows. Actor matches an actor id or an actor type
// ("system"); Action matches by prefix ("setting." finds every setting
// change). From is inclusive, To exclusive; After is an exclusive id cursor.
type AuditFilter struct {
	OrgID, Actor, Action, After string
	From, To                    time.Time
}

// SearchAudit returns matching audit rows newest first.
func SearchAudit(ctx context.Context, q Queryer, f AuditFilter, limit int) ([]AuditRow, error) {
	query := `SELECT id, org_id, actor_type, COALESCE(actor_id,''), action, COALESCE(target,''),
		COALESCE(before,''), COALESCE(after,''), COALESCE(ip,''), at FROM audit_log WHERE org_id = ?`
	args := []any{f.OrgID}
	add := func(cond string, a ...any) {
		query += " AND " + cond
		args = append(args, a...)
	}
	if f.Actor != "" {
		add("(actor_id = ? OR actor_type = ?)", f.Actor, f.Actor)
	}
	if f.Action != "" {
		add(`LOWER(action) LIKE ? ESCAPE '\'`, likeEscape(f.Action)+"%")
	}
	if !f.From.IsZero() {
		add("at >= ?", ms(f.From))
	}
	if !f.To.IsZero() {
		add("at < ?", ms(f.To))
	}
	if f.After != "" {
		add("id < ?", f.After)
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var at int64
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ActorType, &r.ActorID, &r.Action, &r.Target, &r.Before, &r.After, &r.IP, &at); err != nil {
			return nil, err
		}
		r.At = fromMS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- events and deliveries ----

const paymentEventsWhere = `((e.object_type = 'payment' AND e.object_id = ?)
	OR (e.object_type = 'refund' AND e.object_id IN (SELECT id FROM refunds WHERE payment_id = ?)))`

// ListPaymentEvents returns the events about a payment and its refunds,
// oldest first.
func ListPaymentEvents(ctx context.Context, q Queryer, paymentID string) ([]EventRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.id, e.org_id, e.app_id, e.mode, e.type, e.object_type, e.object_id,
		e.object_version, e.data, e.created_at FROM events e WHERE `+paymentEventsWhere+` ORDER BY e.created_at, e.id`, paymentID, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var data string
		var created int64
		if err := rows.Scan(&e.ID, &e.OrgID, &e.AppID, &e.Mode, &e.Type, &e.ObjectType, &e.ObjectID, &e.ObjectVersion, &data, &created); err != nil {
			return nil, err
		}
		e.Data, e.CreatedAt = json.RawMessage(data), fromMS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeliveryListRow is a webhook delivery joined with its event and endpoint.
type DeliveryListRow struct {
	DeliveryID, EventID, EventType, EndpointID, EndpointURL, Status string
	AttemptCount, LastStatus                                        int
	LastError                                                       string
	DeliveredAt, CreatedAt, UpdatedAt                               time.Time
}

const deliveryListSelect = `SELECT d.id, d.event_id, e.type, d.endpoint_id, w.url, d.status, d.attempt_count,
	d.last_status, d.last_error, d.delivered_at, d.created_at, d.updated_at
	FROM webhook_deliveries d JOIN events e ON e.id = d.event_id JOIN webhook_endpoints w ON w.id = d.endpoint_id`

func collectDeliveries(rows *sql.Rows, err error) ([]DeliveryListRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryListRow
	for rows.Next() {
		var r DeliveryListRow
		var last sql.NullInt64
		var lastErr sql.NullString
		var delivered sql.NullInt64
		var created, updated int64
		if err := rows.Scan(&r.DeliveryID, &r.EventID, &r.EventType, &r.EndpointID, &r.EndpointURL, &r.Status, &r.AttemptCount,
			&last, &lastErr, &delivered, &created, &updated); err != nil {
			return nil, err
		}
		r.LastStatus, r.LastError = int(last.Int64), lastErr.String
		r.DeliveredAt, r.CreatedAt, r.UpdatedAt = fromNullMS(delivered), fromMS(created), fromMS(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListPaymentDeliveries returns the deliveries of the events about a payment
// and its refunds, oldest first.
func ListPaymentDeliveries(ctx context.Context, q Queryer, paymentID string) ([]DeliveryListRow, error) {
	return collectDeliveries(q.QueryContext(ctx, deliveryListSelect+" WHERE "+paymentEventsWhere+" ORDER BY d.created_at, d.id", paymentID, paymentID))
}

// ListEndpointDeliveries returns an endpoint's latest deliveries, newest first.
func ListEndpointDeliveries(ctx context.Context, q Queryer, endpointID string, limit int) ([]DeliveryListRow, error) {
	return collectDeliveries(q.QueryContext(ctx, deliveryListSelect+" WHERE d.endpoint_id = ? ORDER BY d.created_at DESC, d.id DESC LIMIT ?", endpointID, limit))
}
