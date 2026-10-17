package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// JobRow is a row of the jobs table.
type JobRow struct {
	ID, Kind              string
	Payload               json.RawMessage
	State                 string
	RunAt                 time.Time
	Attempts, MaxAttempts int
	LockedUntil           time.Time
	LastError, DedupeKey  string
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// InsertJob inserts j inside tx. A dedupe conflict is a silent no-op.
func (db *DB) InsertJob(ctx context.Context, tx Tx, j JobRow) error {
	if j.State == "" {
		j.State = "pending"
	}
	now := time.Now().UnixMilli()
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, kind, payload, state, run_at, attempts, max_attempts, locked_until, last_error, dedupe_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		j.ID, j.Kind, string(j.Payload), j.State, j.RunAt.UnixMilli(), j.Attempts, j.MaxAttempts,
		nullTime(j.LockedUntil), nullStr(j.LastError), nullStr(j.DedupeKey), now, now)
	if err != nil {
		return fmt.Errorf("insert job: %w", err)
	}
	return nil
}

// ClaimJobs leases up to limit runnable jobs (pending and due, or running with
// an expired lease) and increments their attempts. On Postgres the inner
// select skips rows locked by other claimers.
func (db *DB) ClaimJobs(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]JobRow, error) {
	var out []JobRow
	nowMS := now.UnixMilli()
	until := now.Add(lease)
	err := db.Write(ctx, func(tx Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, `UPDATE jobs SET state = 'running', attempts = attempts + 1, locked_until = ?, updated_at = ?
			WHERE id IN (SELECT id FROM jobs
			             WHERE (state = 'pending' AND run_at <= ?) OR (state = 'running' AND locked_until <= ?)
			             ORDER BY run_at LIMIT ?`+db.d.SkipLocked()+`)
			RETURNING id, kind, payload, attempts, max_attempts`,
			until.UnixMilli(), nowMS, nowMS, nowMS, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			j := JobRow{State: "running", LockedUntil: until}
			var payload string
			if err := rows.Scan(&j.ID, &j.Kind, &payload, &j.Attempts, &j.MaxAttempts); err != nil {
				return err
			}
			j.Payload = json.RawMessage(payload)
			out = append(out, j)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	return out, nil
}

// CompleteJob marks the job done, but only if it is still the claim identified
// by attempts (the claim token). It reports whether a row was updated; false
// means a newer claim superseded this worker.
func (db *DB) CompleteJob(ctx context.Context, id string, attempts int, now time.Time) (bool, error) {
	return db.updateClaim(ctx, `UPDATE jobs SET state = 'done', locked_until = NULL, updated_at = ?
		WHERE id = ? AND state = 'running' AND attempts = ?`, now.UnixMilli(), id, attempts)
}

// FailJob records a failure for the claim identified by attempts: dead jobs stay
// dead, others return to pending at nextRunAt. It reports whether a row was
// updated; false means a newer claim superseded this worker.
func (db *DB) FailJob(ctx context.Context, id string, attempts int, now, nextRunAt time.Time, errMsg string, dead bool) (bool, error) {
	if dead {
		return db.updateClaim(ctx, `UPDATE jobs SET state = 'dead', locked_until = NULL, last_error = ?, updated_at = ?
			WHERE id = ? AND state = 'running' AND attempts = ?`, errMsg, now.UnixMilli(), id, attempts)
	}
	return db.updateClaim(ctx, `UPDATE jobs SET state = 'pending', run_at = ?, locked_until = NULL, last_error = ?, updated_at = ?
		WHERE id = ? AND state = 'running' AND attempts = ?`, nextRunAt.UnixMilli(), errMsg, now.UnixMilli(), id, attempts)
}

func (db *DB) updateClaim(ctx context.Context, query string, args ...any) (bool, error) {
	var n int64
	err := db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n > 0, err
}

// JobStats returns the job count per state and the age of the oldest due pending job.
func (db *DB) JobStats(ctx context.Context, now time.Time) (map[string]int, time.Duration, error) {
	depth := map[string]int{}
	var oldest time.Duration
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			var n int
			if err := rows.Scan(&s, &n); err != nil {
				rows.Close()
				return err
			}
			depth[s] = n
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var minRun sql.NullInt64
		if err := q.QueryRowContext(ctx, `SELECT MIN(run_at) FROM jobs WHERE state = 'pending' AND run_at <= ?`, now.UnixMilli()).Scan(&minRun); err != nil {
			return err
		}
		if minRun.Valid {
			oldest = now.Sub(time.UnixMilli(minRun.Int64))
		}
		return nil
	})
	return depth, oldest, err
}
