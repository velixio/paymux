package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaimDueSchedule decides, inside tx, whether the named schedule is due. The
// row is locked (FOR UPDATE on Postgres; SQLite has a single writer), so
// concurrent instances serialise and only the first sees due=true.
//
// A schedule seen for the first time is recorded with next_run_at=defaultNext
// and is not due. When due, runAt is the scheduled instant that was missed.
func (db *DB) ClaimDueSchedule(ctx context.Context, tx Tx, name string, now, defaultNext time.Time) (due bool, runAt time.Time, err error) {
	var next int64
	err = tx.QueryRowContext(ctx, "SELECT next_run_at FROM schedules WHERE name = ?"+db.d.ForUpdate(), name).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO schedules (name, next_run_at, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (name) DO NOTHING`, name, defaultNext.UnixMilli(), now.UnixMilli())
		if err != nil {
			return false, time.Time{}, fmt.Errorf("insert schedule %s: %w", name, err)
		}
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("select schedule %s: %w", name, err)
	}
	if next > now.UnixMilli() {
		return false, time.Time{}, nil
	}
	return true, time.UnixMilli(next), nil
}

// SetScheduleNext records the next run of a schedule claimed in tx.
func (db *DB) SetScheduleNext(ctx context.Context, tx Tx, name string, next, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE schedules SET next_run_at = ?, updated_at = ? WHERE name = ?",
		next.UnixMilli(), now.UnixMilli(), name)
	if err != nil {
		return fmt.Errorf("update schedule %s: %w", name, err)
	}
	return nil
}
