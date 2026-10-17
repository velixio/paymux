package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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
	CreatedAt, UpdatedAt                         time.Time
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
	COALESCE(return_url,''), client_secret_hash, expires_at, version, flags, created_at, updated_at`

func scanPayment(row interface{ Scan(...any) error }) (Payment, error) {
	var p Payment
	var meta, flags string
	var exp, created, updated int64
	err := row.Scan(&p.ID, &p.OrgID, &p.AppID, &p.Mode, &p.Kind, &p.SubscriptionID, &p.Amount, &p.Currency,
		&p.AmountReceived, &p.AmountRefunded, &p.Status, &p.OrderRef, &p.Description, &p.CustomerID, &meta,
		&p.ReturnURL, &p.ClientSecretHash, &exp, &p.Version, &flags, &created, &updated)
	if err != nil {
		return Payment{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(meta), &p.Metadata); err != nil {
		return Payment{}, fmt.Errorf("payment %s metadata: %w", p.ID, err)
	}
	if err := json.Unmarshal([]byte(flags), &p.Flags); err != nil {
		return Payment{}, fmt.Errorf("payment %s flags: %w", p.ID, err)
	}
	p.ExpiresAt, p.CreatedAt, p.UpdatedAt = fromMS(exp), fromMS(created), fromMS(updated)
	return p, nil
}

// InsertPayment inserts p (version 1 is the caller's choice).
func InsertPayment(ctx context.Context, tx Tx, p Payment) error {
	meta, _ := json.Marshal(nonNilMap(p.Metadata))
	flags, _ := json.Marshal(nonNilSlice(p.Flags))
	_, err := tx.ExecContext(ctx, `INSERT INTO payments (id, org_id, app_id, mode, kind, subscription_id, amount, currency,
		amount_received, amount_refunded, status, order_ref, description, customer_id, metadata, return_url,
		client_secret_hash, expires_at, version, flags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.OrgID, p.AppID, p.Mode, p.Kind, nullIfEmpty(p.SubscriptionID), p.Amount, p.Currency,
		p.AmountReceived, p.AmountRefunded, p.Status, nullIfEmpty(p.OrderRef), nullIfEmpty(p.Description),
		nullIfEmpty(p.CustomerID), string(meta), nullIfEmpty(p.ReturnURL), p.ClientSecretHash, ms(p.ExpiresAt),
		p.Version, string(flags), ms(p.CreatedAt), ms(p.UpdatedAt))
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

// ListPayments returns an app's payments in one mode, newest first (ids are
// time-ordered). afterID is an exclusive cursor; status filters when set.
func ListPayments(ctx context.Context, q Queryer, appID, mode, status, afterID string, limit int) ([]Payment, error) {
	query := "SELECT " + paymentCols + " FROM payments WHERE app_id = ? AND mode = ?"
	args := []any{appID, mode}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if afterID != "" {
		query += " AND id < ?"
		args = append(args, afterID)
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
