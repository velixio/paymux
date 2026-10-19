package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrDuplicate means a unique value (e.g. a user's email) already exists.
var ErrDuplicate = errors.New("already exists")

// UserRow is a dashboard user. TOTPSecretEnc is nil until a secret is
// generated; TOTPLastStep is -1 until the first code is accepted, which is how
// "enrolled" is told apart from "secret shown but never confirmed".
type UserRow struct {
	ID, Email, PasswordHash string
	TOTPSecretEnc           []byte
	TOTPLastStep            int64
	Role                    string // a built-in (admin, operator, viewer) or custom role name
	Disabled                bool
	CreatedAt               time.Time
}

const userCols = `id, email, password_hash, totp_secret_enc, totp_last_step, role, disabled, created_at`

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func scanUser(s scanner) (UserRow, error) {
	var u UserRow
	var disabled int
	var created int64
	if err := s.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.TOTPSecretEnc, &u.TOTPLastStep, &u.Role, &disabled, &created); err != nil {
		return UserRow{}, err
	}
	if len(u.TOTPSecretEnc) == 0 {
		u.TOTPSecretEnc = nil
	}
	u.Disabled, u.CreatedAt = disabled != 0, fromMS(created)
	return u, nil
}

// InsertUser creates a user, or returns ErrDuplicate if the email is taken.
func (db *DB) InsertUser(ctx context.Context, u UserRow) error {
	u.Email = normEmail(u.Email)
	return db.Write(ctx, func(tx Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE email = ?`, u.Email).Scan(&one)
		if err == nil {
			return ErrDuplicate
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if ok, err := roleKnown(ctx, tx, u.Role); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownRole, u.Role)
		}
		var enc any
		if u.TOTPSecretEnc != nil {
			enc = u.TOTPSecretEnc
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, u.Email, u.PasswordHash, enc, u.TOTPLastStep, u.Role, b2i(u.Disabled), ms(u.CreatedAt))
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}
		return nil
	})
}

func (db *DB) oneUser(ctx context.Context, where string, arg any) (UserRow, error) {
	var u UserRow
	err := db.Read(ctx, func(q Querier) (err error) {
		u, err = scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE `+where, arg))
		return
	})
	if errors.Is(err, sql.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	return u, err
}

// UserByEmail finds a user by (case-insensitive) email, or ErrNotFound.
func (db *DB) UserByEmail(ctx context.Context, email string) (UserRow, error) {
	return db.oneUser(ctx, `email = ?`, normEmail(email))
}

// UserByID finds a user by id, or ErrNotFound.
func (db *DB) UserByID(ctx context.Context, id string) (UserRow, error) {
	return db.oneUser(ctx, `id = ?`, id)
}

// ListUsers returns all users by creation time.
func (db *DB) ListUsers(ctx context.Context) ([]UserRow, error) {
	var out []UserRow
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}

// CountUsers returns the number of users.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n) })
	return n, err
}

// CountEnabledAdmins returns the number of enabled admins.
func (db *DB) CountEnabledAdmins(ctx context.Context) (int, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	})
	return n, err
}

func (db *DB) updateUser(ctx context.Context, id, set string, args ...any) error {
	return db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET `+set+` WHERE id = ?`, append(args, id)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetUserRole changes a user's role.
func (db *DB) SetUserRole(ctx context.Context, id, role string) error {
	return db.Write(ctx, func(tx Tx) error {
		if ok, err := roleKnown(ctx, tx, role); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownRole, role)
		}
		res, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetUserDisabled disables or re-enables a user.
func (db *DB) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	return db.updateUser(ctx, id, `disabled = ?`, b2i(disabled))
}

// SetUserTOTP stores the encrypted TOTP secret.
func (db *DB) SetUserTOTP(ctx context.Context, id string, enc []byte) error {
	return db.updateUser(ctx, id, `totp_secret_enc = ?`, enc)
}

// ResetUserTOTP clears the secret and the replay step so the user enrols again.
func (db *DB) ResetUserTOTP(ctx context.Context, id string) error {
	if err := db.updateUser(ctx, id, `totp_secret_enc = NULL, totp_last_step = -1`); err != nil {
		return err
	}
	return db.ClearEnrolmentToken(ctx, id) // a code issued for the old enrolment is void
}

// SetUserPassword replaces the password hash.
func (db *DB) SetUserPassword(ctx context.Context, id, hash string) error {
	return db.updateUser(ctx, id, `password_hash = ?`, hash)
}

// AdvanceTOTPStep records step as used, only if it is newer than the last
// accepted one. It reports whether it advanced; false means a replay.
func (db *DB) AdvanceTOTPStep(ctx context.Context, id string, step int64) (bool, error) {
	var ok bool
	err := db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, step, id, step)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n > 0
		return nil
	})
	return ok, err
}

// SessionRow is a dashboard session. Only the hash of the cookie token is stored.
type SessionRow struct {
	ID, UserID, TokenHash            string
	CSRFKey                          []byte
	CreatedAt, LastSeenAt, ExpiresAt time.Time
	IP, UserAgent                    string
}

// InsertSession stores a new session.
func (db *DB) InsertSession(ctx context.Context, s SessionRow) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(id, user_id, token_hash, csrf_key, created_at, last_seen_at, expires_at, ip, user_agent)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.UserID, s.TokenHash, s.CSRFKey, ms(s.CreatedAt), ms(s.LastSeenAt), ms(s.ExpiresAt), s.IP, s.UserAgent)
		if err != nil {
			return fmt.Errorf("insert session: %w", err)
		}
		return nil
	})
}

// SessionByTokenHash returns the session and its user, or ErrNotFound.
func (db *DB) SessionByTokenHash(ctx context.Context, hash string) (SessionRow, UserRow, error) {
	var s SessionRow
	var u UserRow
	err := db.Read(ctx, func(q Querier) error {
		var created, seen, exp, ucreated int64
		var disabled int
		err := q.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.token_hash, s.csrf_key, s.created_at, s.last_seen_at, s.expires_at, s.ip, s.user_agent,
			u.id, u.email, u.password_hash, u.totp_secret_enc, u.totp_last_step, u.role, u.disabled, u.created_at
			FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, hash).
			Scan(&s.ID, &s.UserID, &s.TokenHash, &s.CSRFKey, &created, &seen, &exp, &s.IP, &s.UserAgent,
				&u.ID, &u.Email, &u.PasswordHash, &u.TOTPSecretEnc, &u.TOTPLastStep, &u.Role, &disabled, &ucreated)
		if err != nil {
			return err
		}
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt = fromMS(created), fromMS(seen), fromMS(exp)
		u.Disabled, u.CreatedAt = disabled != 0, fromMS(ucreated)
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRow{}, UserRow{}, ErrNotFound
	}
	return s, u, err
}

// TouchSession records activity.
func (db *DB) TouchSession(ctx context.Context, id string, at time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`, ms(at), id)
		return err
	})
}

// DeleteSession removes one session (logout).
func (db *DB) DeleteSession(ctx context.Context, id string) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		return err
	})
}

// DeleteUserSessions removes every session of a user.
func (db *DB) DeleteUserSessions(ctx context.Context, userID string) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
		return err
	})
}

// DeleteExpiredSessions removes sessions whose absolute expiry has passed.
func (db *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, ms(now))
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}
