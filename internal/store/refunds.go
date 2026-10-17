package store

import (
	"context"
	"time"
)

// Refund is a row of refunds.
type Refund struct {
	ID, OrgID, AppID, Mode, PaymentID, AttemptID string
	Amount                                       int64
	Currency, Status, Reason, ProviderRef        string
	ProviderRefundID, InitiatedBy, Actor         string
	CreatedAt, UpdatedAt                         time.Time
}

const refundCols = `id, org_id, app_id, mode, payment_id, attempt_id, amount, currency, status, COALESCE(reason,''),
	provider_ref, COALESCE(provider_refund_id,''), initiated_by, COALESCE(actor,''), created_at, updated_at`

func scanRefund(row interface{ Scan(...any) error }) (Refund, error) {
	var r Refund
	var created, updated int64
	err := row.Scan(&r.ID, &r.OrgID, &r.AppID, &r.Mode, &r.PaymentID, &r.AttemptID, &r.Amount, &r.Currency, &r.Status,
		&r.Reason, &r.ProviderRef, &r.ProviderRefundID, &r.InitiatedBy, &r.Actor, &created, &updated)
	if err != nil {
		return Refund{}, notFound(err)
	}
	r.CreatedAt, r.UpdatedAt = fromMS(created), fromMS(updated)
	return r, nil
}

// InsertRefund inserts r.
func InsertRefund(ctx context.Context, tx Tx, r Refund) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO refunds (id, org_id, app_id, mode, payment_id, attempt_id, amount, currency,
		status, reason, provider_ref, provider_refund_id, initiated_by, actor, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.OrgID, r.AppID, r.Mode, r.PaymentID, r.AttemptID, r.Amount, r.Currency, r.Status,
		nullIfEmpty(r.Reason), r.ProviderRef, nullIfEmpty(r.ProviderRefundID), r.InitiatedBy,
		nullIfEmpty(r.Actor), ms(r.CreatedAt), ms(r.UpdatedAt))
	return err
}

// GetRefund loads a refund by id.
func GetRefund(ctx context.Context, q Queryer, id string) (Refund, error) {
	return scanRefund(q.QueryRowContext(ctx, "SELECT "+refundCols+" FROM refunds WHERE id = ?", id))
}

// ListRefunds returns the refunds of a payment, oldest first.
func ListRefunds(ctx context.Context, q Queryer, paymentID string) ([]Refund, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+refundCols+" FROM refunds WHERE payment_id = ? ORDER BY id", paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Refund
	for rows.Next() {
		r, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SumActiveRefunds returns the total of pending and succeeded refunds of an
// attempt: the amount that is no longer refundable.
func SumActiveRefunds(ctx context.Context, q Queryer, attemptID string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM refunds
		WHERE attempt_id = ? AND status IN ('pending','succeeded')`, attemptID).Scan(&n)
	return n, err
}
