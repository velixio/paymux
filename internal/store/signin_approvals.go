package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A sign-in approval request is pending until a reply decides it or its time
// runs out. An approved request is collected once by the browser that started
// it. The PIN and the reply text are never stored here.
const (
	SigninPending   = "pending"
	SigninApproved  = "approved"
	SigninCollected = "collected"
	SigninEnded     = "ended"
)

// SigninApproval is one sign-in waiting for, or decided by, an approval.
type SigninApproval struct {
	ID, OrgID, UserID, BrowserHash, IP, Status string
	Remember                                   bool
	ChatID, MessageID                          int64 // 0 until the notification is sent
	WrongAttempts                              int
	Cleared                                    bool // the notification has been deleted
	CreatedAt, ExpiresAt, DecidedAt            time.Time
}

const signinCols = `id, org_id, user_id, browser_hash, ip, status, remember, chat_id, message_id, wrong_attempts, cleared, created_at, expires_at, decided_at`

func scanSignin(s scanner) (SigninApproval, error) {
	var a SigninApproval
	var remember, cleared, created, expires, decided int64
	if err := s.Scan(&a.ID, &a.OrgID, &a.UserID, &a.BrowserHash, &a.IP, &a.Status, &remember, &a.ChatID, &a.MessageID,
		&a.WrongAttempts, &cleared, &created, &expires, &decided); err != nil {
		return SigninApproval{}, err
	}
	a.Remember, a.Cleared = remember != 0, cleared != 0
	a.CreatedAt, a.ExpiresAt, a.DecidedAt = fromMS(created), fromMS(expires), fromMSZero(decided)
	return a, nil
}

func endPendingSignins(ctx context.Context, tx Tx, userID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET status = ?, decided_at = ? WHERE user_id = ? AND status = ?`,
		SigninEnded, ms(now), userID, SigninPending)
	return err
}

func (db *DB) setUserSigninPIN(ctx context.Context, userID string, enc any, now time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET signin_pin_enc = ? WHERE id = ?`, enc, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return endPendingSignins(ctx, tx, userID, now)
	})
}

// SetUserSigninPIN stores the sealed PIN and ends the user's pending requests.
func (db *DB) SetUserSigninPIN(ctx context.Context, userID string, enc []byte, now time.Time) error {
	return db.setUserSigninPIN(ctx, userID, enc, now)
}

// ClearUserSigninPIN removes the PIN and ends the user's pending requests.
func (db *DB) ClearUserSigninPIN(ctx context.Context, userID string, now time.Time) error {
	return db.setUserSigninPIN(ctx, userID, nil, now)
}

// InsertSigninApproval stores a new request. A user has one request at a time,
// so this first ends the user's earlier pending ones.
func InsertSigninApproval(ctx context.Context, tx Tx, a SigninApproval) error {
	if err := endPendingSignins(ctx, tx, a.UserID, a.CreatedAt); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO signin_approvals (id, org_id, user_id, browser_hash, ip, status, remember, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OrgID, a.UserID, a.BrowserHash, a.IP, a.Status, b2i(a.Remember), ms(a.CreatedAt), ms(a.ExpiresAt))
	return err
}

func (db *DB) oneSignin(ctx context.Context, where string, args ...any) (SigninApproval, error) {
	var a SigninApproval
	err := db.Read(ctx, func(q Querier) (err error) {
		a, err = scanSignin(q.QueryRowContext(ctx, `SELECT `+signinCols+` FROM signin_approvals WHERE `+where, args...))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return SigninApproval{}, ErrNotFound
	}
	return a, err
}

func (db *DB) listSignins(ctx context.Context, where string, args ...any) ([]SigninApproval, error) {
	var out []SigninApproval
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT `+signinCols+` FROM signin_approvals WHERE `+where+` ORDER BY created_at, id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanSignin(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// SigninApprovalByBrowser finds the request a browser's cookie stands for, or
// ErrNotFound.
func (db *DB) SigninApprovalByBrowser(ctx context.Context, browserHash string) (SigninApproval, error) {
	return db.oneSignin(ctx, `browser_hash = ?`, browserHash)
}

// SigninApprovalByNotification finds the request a Telegram message belongs to,
// or ErrNotFound.
func (db *DB) SigninApprovalByNotification(ctx context.Context, chatID, messageID int64) (SigninApproval, error) {
	return db.oneSignin(ctx, `chat_id = ? AND message_id = ?`, chatID, messageID)
}

// CollectSigninApproval turns an approved request into a collected one, once. It
// reports false when the request is not approved, or was decided before
// decidedAfter.
func (db *DB) CollectSigninApproval(ctx context.Context, id string, decidedAfter time.Time) (bool, error) {
	var ok bool
	err := db.Write(ctx, func(tx Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET status = ? WHERE id = ? AND status = ? AND decided_at >= ?`,
			SigninCollected, id, SigninApproved, ms(decidedAfter))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n > 0
		return nil
	})
	return ok, err
}

// WrongSigninPINsSince counts the wrong PINs sent for a user's requests created
// at or after since.
func (db *DB) WrongSigninPINsSince(ctx context.Context, userID string, since time.Time) (int, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT CAST(COALESCE(SUM(wrong_attempts), 0) AS BIGINT) FROM signin_approvals WHERE user_id = ? AND created_at >= ?`,
			userID, ms(since)).Scan(&n)
	})
	return n, err
}

// ExpireSigninApprovals ends the pending requests whose time has run out.
func (db *DB) ExpireSigninApprovals(ctx context.Context, now time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET status = ?, decided_at = ? WHERE status = ? AND expires_at <= ?`,
			SigninEnded, ms(now), SigninPending, ms(now))
		return err
	})
}

// SigninApprovalsToSend lists the live requests whose notification is not sent yet.
func (db *DB) SigninApprovalsToSend(ctx context.Context, now time.Time) ([]SigninApproval, error) {
	return db.listSignins(ctx, `status = ? AND message_id = 0 AND expires_at > ?`, SigninPending, ms(now))
}

// SetSigninNotification records the message that asks for approval.
func (db *DB) SetSigninNotification(ctx context.Context, id string, chatID, messageID int64) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET chat_id = ?, message_id = ? WHERE id = ?`, chatID, messageID, id)
		return err
	})
}

// SigninApprovalsToClear lists the decided requests whose notification is still
// in the chat.
func (db *DB) SigninApprovalsToClear(ctx context.Context) ([]SigninApproval, error) {
	return db.listSignins(ctx, `status <> ? AND message_id <> 0 AND cleared = 0`, SigninPending)
}

// MarkSigninCleared records that a request's notification is gone.
func (db *DB) MarkSigninCleared(ctx context.Context, id string) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET cleared = 1 WHERE id = ?`, id)
		return err
	})
}

// RecordSigninReply applies one reply to a live request: a correct one approves
// it, and the maxWrong-th wrong one ends it. A request already decided or out of
// time does not change. It returns the request as it now stands.
func (db *DB) RecordSigninReply(ctx context.Context, id string, correct bool, maxWrong int, now time.Time) (SigninApproval, error) {
	err := db.Write(ctx, func(tx Tx) error {
		if correct {
			_, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET status = ?, decided_at = ? WHERE id = ? AND status = ? AND expires_at > ?`,
				SigninApproved, ms(now), id, SigninPending, ms(now))
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE signin_approvals SET wrong_attempts = wrong_attempts + 1 WHERE id = ? AND status = ? AND expires_at > ?`,
			id, SigninPending, ms(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE signin_approvals SET status = ?, decided_at = ? WHERE id = ? AND status = ? AND wrong_attempts >= ?`,
			SigninEnded, ms(now), id, SigninPending, maxWrong)
		return err
	})
	if err != nil {
		return SigninApproval{}, err
	}
	return db.oneSignin(ctx, `id = ?`, id)
}

// ClaimSigninPoll takes the right to read one bot's replies until until. ok is
// false while someone else holds it. offset is where reading continues; it
// belongs to this bot, as update ids from another bot would not be comparable.
func (db *DB) ClaimSigninPoll(ctx context.Context, botID string, now, until time.Time) (offset int64, ok bool, err error) {
	err = db.Write(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO signin_polls (bot_id) VALUES (?) ON CONFLICT (bot_id) DO NOTHING`, botID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE signin_polls SET lease_until = ? WHERE bot_id = ? AND lease_until <= ?`, ms(until), botID, ms(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		return tx.QueryRowContext(ctx, `SELECT update_offset FROM signin_polls WHERE bot_id = ?`, botID).Scan(&offset)
	})
	return offset, ok, err
}

// ReleaseSigninPoll saves how far a bot's replies were read and gives up the claim.
func (db *DB) ReleaseSigninPoll(ctx context.Context, botID string, offset int64) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE signin_polls SET update_offset = ?, lease_until = 0 WHERE bot_id = ?`, offset, botID)
		return err
	})
}

// HasOpenSigninApprovals reports whether anything is left to send, read or delete.
func (db *DB) HasOpenSigninApprovals(ctx context.Context) (bool, error) {
	var n int
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM signin_approvals WHERE status = ? OR (message_id <> 0 AND cleared = 0)`, SigninPending).Scan(&n)
	})
	return n > 0, err
}

// PendingSigninOrgs lists the organizations with a pending request.
func (db *DB) PendingSigninOrgs(ctx context.Context) ([]string, error) {
	var out []string
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT org_id FROM signin_approvals WHERE status = ? ORDER BY org_id`, SigninPending)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// PurgeSigninApprovals deletes the decided requests created before before.
func (db *DB) PurgeSigninApprovals(ctx context.Context, before time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM signin_approvals WHERE status <> ? AND created_at < ?`, SigninPending, ms(before))
		return err
	})
}
