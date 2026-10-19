package store

import (
	"context"
	"errors"
	"time"
)

// ErrIdempotencyNotOwner means the reservation is no longer the caller's: it was
// taken over as stale, or released.
var ErrIdempotencyNotOwner = errors.New("store: the idempotency key is held by another request")

// IdempotencyRecord is a row of idempotency_keys. Status 0 means the first
// request is still in progress.
type IdempotencyRecord struct {
	RequestHash string
	Status      int
	ResponseEnc []byte
	CreatedAt   time.Time
}

// ReserveIdempotencyKey claims (app, mode, key) for the request identified by
// the owner token. It returns nil when the caller now owns the key. Otherwise it returns the existing record.
// Records older than ttl, and in-progress records older than staleAfter, are
// discarded first.
func ReserveIdempotencyKey(ctx context.Context, tx Tx, appID, mode, key, owner, hash string, now time.Time, ttl, staleAfter time.Duration) (*IdempotencyRecord, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE app_id = ? AND mode = ? AND key = ?
		AND (created_at < ? OR (response_status = 0 AND created_at < ?))`,
		appID, mode, key, ms(now.Add(-ttl)), ms(now.Add(-staleAfter))); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (app_id, mode, key, request_hash, response_status, created_at, owner)
		VALUES (?, ?, ?, ?, 0, ?, ?) ON CONFLICT (app_id, mode, key) DO NOTHING`, appID, mode, key, hash, ms(now), owner)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, nil
	}
	var rec IdempotencyRecord
	var created int64
	err = tx.QueryRowContext(ctx, `SELECT request_hash, response_status, response_enc, created_at FROM idempotency_keys
		WHERE app_id = ? AND mode = ? AND key = ?`, appID, mode, key).Scan(&rec.RequestHash, &rec.Status, &rec.ResponseEnc, &created)
	if err != nil {
		return nil, err
	}
	rec.CreatedAt = fromMS(created)
	return &rec, nil
}

// CompleteIdempotencyKey stores the response of the owning request. It returns
// ErrIdempotencyNotOwner when the owner token does not hold the reservation.
func CompleteIdempotencyKey(ctx context.Context, tx Tx, appID, mode, key, owner string, status int, responseEnc []byte) error {
	res, err := tx.ExecContext(ctx, `UPDATE idempotency_keys SET response_status = ?, response_enc = ?
		WHERE app_id = ? AND mode = ? AND key = ? AND owner = ?`, status, responseEnc, appID, mode, key, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrIdempotencyNotOwner
	}
	return nil
}

// ReleaseIdempotencyKey drops the owner's reservation so the client can retry.
// A reservation held by another request is left alone.
func ReleaseIdempotencyKey(ctx context.Context, tx Tx, appID, mode, key, owner string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM idempotency_keys WHERE app_id = ? AND mode = ? AND key = ? AND owner = ?", appID, mode, key, owner)
	return err
}

// PurgeIdempotencyKeys deletes keys created before cutoff.
func PurgeIdempotencyKeys(ctx context.Context, tx Tx, cutoff time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, "DELETE FROM idempotency_keys WHERE created_at < ?", ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
