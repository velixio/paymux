package store

import (
	"context"
	"time"
)

// SetEnrolmentToken stores the hash of the one-time code that lets the user set
// up two-factor sign-in, replacing any earlier code.
func (db *DB) SetEnrolmentToken(ctx context.Context, userID, tokenHash string, expires time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_enrolment_tokens WHERE user_id = ?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO user_enrolment_tokens (user_id, token_hash, expires_at, unlocked_until) VALUES (?, ?, ?, 0)`,
			userID, tokenHash, ms(expires))
		return err
	})
}

// UnlockEnrolment opens the enrolment of the user for window when the hash is
// the stored, unexpired one. It reports whether it did.
func (db *DB) UnlockEnrolment(ctx context.Context, userID, tokenHash string, now time.Time, window time.Duration) (bool, error) {
	var ok bool
	err := db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE user_enrolment_tokens SET unlocked_until = ?
			WHERE user_id = ? AND token_hash = ? AND expires_at > ?`, ms(now.Add(window)), userID, tokenHash, ms(now))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n == 1
		return nil
	})
	return ok, err
}

// EnrolmentUnlocked reports whether the user may be shown the secret now.
func (db *DB) EnrolmentUnlocked(ctx context.Context, userID string, now time.Time) (bool, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_enrolment_tokens WHERE user_id = ? AND unlocked_until > ? AND expires_at > ?`,
			userID, ms(now), ms(now)).Scan(&n)
	})
	return n > 0, err
}

// HasEnrolmentToken reports whether the user has a code (spent or not yet).
func (db *DB) HasEnrolmentToken(ctx context.Context, userID string) (bool, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_enrolment_tokens WHERE user_id = ?`, userID).Scan(&n)
	})
	return n > 0, err
}

// ClearEnrolmentToken drops the user's code, after enrolment.
func (db *DB) ClearEnrolmentToken(ctx context.Context, userID string) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM user_enrolment_tokens WHERE user_id = ?`, userID)
		return err
	})
}
