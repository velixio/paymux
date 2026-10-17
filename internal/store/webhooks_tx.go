package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GetDeliveryTx loads one delivery row inside tx, or returns ErrNotFound.
func (db *DB) GetDeliveryTx(ctx context.Context, tx Tx, id string) (DeliveryRow, error) {
	var (
		d                  DeliveryRow
		next, delivered    sql.NullInt64
		lastStatus         sql.NullInt64
		lastErr            sql.NullString
		created, updatedMS int64
	)
	err := tx.QueryRowContext(ctx, `SELECT id, event_id, endpoint_id, status, attempt_count, next_attempt_at,
		last_status, last_error, delivered_at, created_at, updated_at FROM webhook_deliveries WHERE id = ?`, id).
		Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Status, &d.AttemptCount, &next, &lastStatus, &lastErr, &delivered, &created, &updatedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryRow{}, ErrNotFound
	}
	if err != nil {
		return DeliveryRow{}, err
	}
	d.NextAttemptAt, d.DeliveredAt = fromNullMS(next), fromNullMS(delivered)
	d.LastStatus, d.LastError = int(lastStatus.Int64), lastErr.String
	d.CreatedAt, d.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updatedMS).UTC()
	return d, nil
}

// FirstOrgID returns the id of the earliest-created org (Path 1 has exactly
// one), or ErrNotFound when there is none.
func (db *DB) FirstOrgID(ctx context.Context) (string, error) {
	var id string
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT id FROM orgs ORDER BY created_at, id LIMIT 1`).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}
