package store

import (
	"context"
	"time"
)

// OAuthState is an OAuth authorization in flight: the random state sent to the
// provider and back. Only its hash is stored; it is single use and expires.
type OAuthState struct {
	Hash, OrgID, UserID, AccountID string
	ExpiresAt                      time.Time
}

// InsertOAuthState stores a state, first purging those that have expired.
func InsertOAuthState(ctx context.Context, tx Tx, st OAuthState, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM oauth_states WHERE expires_at <= ?", ms(now)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO oauth_states (state_hash, org_id, user_id, account_id, expires_at) VALUES (?, ?, ?, ?, ?)`,
		st.Hash, st.OrgID, st.UserID, st.AccountID, ms(st.ExpiresAt))
	return err
}

// ConsumeOAuthState removes and returns the state; ErrNotFound when it is
// unknown, was used already, or has expired (an expired one is removed too).
func ConsumeOAuthState(ctx context.Context, tx Tx, hash string, now time.Time) (OAuthState, error) {
	st := OAuthState{Hash: hash}
	var exp int64
	err := tx.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state_hash = ? RETURNING org_id, user_id, account_id, expires_at`, hash).
		Scan(&st.OrgID, &st.UserID, &st.AccountID, &exp)
	if err != nil {
		return OAuthState{}, notFound(err)
	}
	st.ExpiresAt = fromMS(exp)
	if !st.ExpiresAt.After(now) {
		return OAuthState{}, ErrNotFound
	}
	return st, nil
}
