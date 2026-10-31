package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RoutingUsage returns captured, non-duplicate sales and their immutable
// ledger amount in one currency since the start of a quota period.
func RoutingUsage(ctx context.Context, q Queryer, accountIDs []string, currency string, since time.Time) (sales, amount int64, err error) {
	if len(accountIDs) == 0 {
		return 0, 0, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(accountIDs)), ",")
	args := make([]any, 0, len(accountIDs)+2)
	for _, id := range accountIDs {
		args = append(args, id)
	}
	args = append(args, currency, ms(since))
	err = q.QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.payment_id), COALESCE(SUM(l.amount), 0)
		FROM payment_attempts a JOIN ledger_entries l ON l.attempt_id = a.id AND l.kind = 'charge'
		WHERE a.provider_account_id IN (`+marks+`) AND a.status = 'captured' AND a.duplicate = 0
		AND l.currency = ? AND l.occurred_at >= ?`, args...).Scan(&sales, &amount)
	return
}

// Payment is a row of payments.
type Payment struct {
	ID, OrgID, AppID, Mode, Kind, SubscriptionID string
	Amount                                       int64
	Currency                                     string
	AmountReceived, AmountRefunded               int64
	Status                                       string
	OrderRef, Description, CustomerID            string
	Metadata                                     map[string]string
	ReturnURL                                    string
	ClientSecretHash                             string
	ExpiresAt                                    time.Time
	Version                                      int64
	Flags                                        []string
	// AllowedConnectors and Country are the routing restrictions the payment was
	// created with; every routing decision for the payment honours them.
	AllowedConnectors    []string
	Country              string
	CreatedAt, UpdatedAt time.Time
}

// HasFlag reports whether the payment carries flag f.
func (p Payment) HasFlag(f string) bool {
	for _, x := range p.Flags {
		if x == f {
			return true
		}
	}
	return false
}

const paymentCols = `id, org_id, app_id, mode, kind, COALESCE(subscription_id,''), amount, currency, amount_received,
	amount_refunded, status, COALESCE(order_ref,''), COALESCE(description,''), COALESCE(customer_id,''), metadata,
	COALESCE(return_url,''), client_secret_hash, expires_at, version, flags, allowed_connectors, country, created_at, updated_at`

func scanPayment(row interface{ Scan(...any) error }) (Payment, error) {
	var p Payment
	var meta, flags, allowed string
	var exp, created, updated int64
	err := row.Scan(&p.ID, &p.OrgID, &p.AppID, &p.Mode, &p.Kind, &p.SubscriptionID, &p.Amount, &p.Currency,
		&p.AmountReceived, &p.AmountRefunded, &p.Status, &p.OrderRef, &p.Description, &p.CustomerID, &meta,
		&p.ReturnURL, &p.ClientSecretHash, &exp, &p.Version, &flags, &allowed, &p.Country, &created, &updated)
	if err != nil {
		return Payment{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(meta), &p.Metadata); err != nil {
		return Payment{}, fmt.Errorf("payment %s metadata: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(flags), &p.Flags); err != nil {
		return Payment{}, fmt.Errorf("payment %s flags: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(allowed), &p.AllowedConnectors); err != nil {
		return Payment{}, fmt.Errorf("payment %s allowed_connectors: %w", p.ID, err)
	}
	p.ExpiresAt, p.CreatedAt, p.UpdatedAt = fromMS(exp), fromMS(created), fromMS(updated)
	return p, nil
}

// InsertPayment inserts p (version 1 is the caller's choice).
func InsertPayment(ctx context.Context, tx Tx, p Payment) error {
	meta, _ := json.Marshal(nonNilMap(p.Metadata))
	flags, _ := json.Marshal(nonNilSlice(p.Flags))
	allowed, _ := json.Marshal(nonNilSlice(p.AllowedConnectors))
	_, err := tx.ExecContext(ctx, `INSERT INTO payments (id, org_id, app_id, mode, kind, subscription_id, amount, currency,
		amount_received, amount_refunded, status, order_ref, description, customer_id, metadata, return_url,
		client_secret_hash, expires_at, version, flags, allowed_connectors, country, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OrgID, p.AppID, p.Mode, p.Kind, nullIfEmpty(p.SubscriptionID), p.Amount, p.Currency,
		p.AmountReceived, p.AmountRefunded, p.Status, nullIfEmpty(p.OrderRef), nullIfEmpty(p.Description),
		nullIfEmpty(p.CustomerID), string(meta), nullIfEmpty(p.ReturnURL), p.ClientSecretHash, ms(p.ExpiresAt),
		p.Version, string(flags), string(allowed), p.Country, ms(p.CreatedAt), ms(p.UpdatedAt))
	return err
}

// GetPayment loads a payment by id.
func GetPayment(ctx context.Context, q Queryer, id string) (Payment, error) {
	return scanPayment(q.QueryRowContext(ctx, "SELECT "+paymentCols+" FROM payments WHERE id = ?", id))
}

// GetPaymentForUpdate loads a payment and, on Postgres, locks the row until
// the transaction ends. SQLite's single writer already serialises.
func GetPaymentForUpdate(ctx context.Context, tx Tx, id string) (Payment, error) {
	return scanPayment(tx.QueryRowContext(ctx, "SELECT "+paymentCols+" FROM payments WHERE id = ?"+tx.Dialect().ForUpdate(), id))
}

// UpdatePayment writes the mutable fields of p if its stored version is still
// p.Version, then bumps the version (p.Version is updated to the new value).
// A stale version returns ErrConflict.
func UpdatePayment(ctx context.Context, tx Tx, p *Payment, now time.Time) error {
	flags, _ := json.Marshal(nonNilSlice(p.Flags))
	res, err := tx.ExecContext(ctx, `UPDATE payments SET status = ?, amount_received = ?, amount_refunded = ?, flags = ?,
		version = version + 1, updated_at = ? WHERE id = ? AND version = ?`,
		p.Status, p.AmountReceived, p.AmountRefunded, string(flags), ms(now), p.ID, p.Version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	p.Version++
	p.UpdatedAt = now
	return nil
}

// ListPayments returns an app's payments in one mode, newest first. afterID is
// an exclusive cursor; status filters when set. Payment IDs are random, so the
// cursor resolves the row's creation time and uses the ID only as a stable
// tie-breaker.
func ListPayments(ctx context.Context, q Queryer, appID, mode, status, afterID string, limit int) ([]Payment, error) {
	query := "SELECT " + paymentCols + " FROM payments WHERE app_id = ? AND mode = ?"
	args := []any{appID, mode}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if afterID != "" {
		var created int64
		err := q.QueryRowContext(ctx, "SELECT created_at FROM payments WHERE id = ? AND app_id = ? AND mode = ?", afterID, appID, mode).Scan(&created)
		if err == sql.ErrNoRows {
			return []Payment{}, nil
		}
		if err != nil {
			return nil, err
		}
		query += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, created, created, afterID)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
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

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNilSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ListExpirablePayments returns up to limit payment ids that are created or
// requires_action, past expires_at at now, and have no open attempt. An open
// attempt is pending or authorized, or created with a provider reference,
// and was created after openSince (unix ms): older ones are no longer polled.
func ListExpirablePayments(ctx context.Context, q Queryer, now time.Time, openSince int64, limit int) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id FROM payments p
		WHERE p.status IN ('created','requires_action') AND p.expires_at < ?
		AND NOT EXISTS (SELECT 1 FROM payment_attempts a WHERE a.payment_id = p.id AND a.created_at > ?
			AND (a.status IN ('pending','authorized')
				OR (a.status = 'created' AND (a.provider_order_id IS NOT NULL OR a.provider_payment_id IS NOT NULL))))
		ORDER BY p.expires_at, p.id LIMIT ?`, ms(now), openSince, limit)
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

// SetPaymentSecretHash replaces the client secret hash of a payment.
func SetPaymentSecretHash(ctx context.Context, tx Tx, id, hash string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE payments SET client_secret_hash = ?, updated_at = ? WHERE id = ?`, hash, ms(now), id)
	return err
}
