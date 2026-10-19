package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UserByIdentity finds the user bound to an identity provider's (issuer,
// subject), or ErrNotFound.
func (db *DB) UserByIdentity(ctx context.Context, issuer, subject string) (UserRow, error) {
	var id string
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT user_id FROM user_identities WHERE issuer = ? AND subject = ?`, issuer, subject).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, err
	}
	return db.UserByID(ctx, id)
}

// UserHasIdentity reports whether any identity is bound to the user.
func (db *DB) UserHasIdentity(ctx context.Context, userID string) (bool, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_identities WHERE user_id = ?`, userID).Scan(&n)
	})
	return n > 0, err
}

// BindIdentity ties an (issuer, subject) to the user and spends the admin's
// permission to link the account, if there was one. ErrDuplicate means the
// identity is already bound.
func (db *DB) BindIdentity(ctx context.Context, issuer, subject, userID string, now time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id, created_at) VALUES (?, ?, ?, ?)`,
			issuer, subject, userID, ms(now)); err != nil {
			if IsUniqueViolation(err) {
				return ErrDuplicate
			}
			return fmt.Errorf("bind identity: %w", err)
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM user_sso_links WHERE user_id = ?`, userID)
		return err
	})
}

// AllowSSOLink records that an admin lets the user's password account be reached
// by single sign-on, once.
func (db *DB) AllowSSOLink(ctx context.Context, userID, byUserID string, now time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_sso_links WHERE user_id = ?`, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO user_sso_links (user_id, allowed_by, created_at) VALUES (?, ?, ?)`, userID, byUserID, ms(now))
		return err
	})
}

// SSOLinkAllowed reports whether an admin has allowed the account to be linked.
func (db *DB) SSOLinkAllowed(ctx context.Context, userID string) (bool, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_sso_links WHERE user_id = ?`, userID).Scan(&n)
	})
	return n > 0, err
}
