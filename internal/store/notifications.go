package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// NotificationConfig is the per-organization, per-mode operator notification
// channel configuration. TelegramTokenEnc is always encrypted at rest.
type NotificationConfig struct {
	OrgID, Mode, Email, TelegramChatID string
	EmailEnabled, TelegramEnabled      bool
	TelegramTokenEnc                   []byte
	TelegramDeleteAfterHours           int
	EventTypes                         []string
	UpdatedAt                          time.Time
}

func GetNotificationConfig(ctx context.Context, q Queryer, orgID, mode string) (NotificationConfig, error) {
	var c NotificationConfig
	var emailOn, telegramOn int
	var events string
	var updated int64
	err := q.QueryRowContext(ctx, `SELECT org_id, mode, COALESCE(email,''), email_enabled,
		COALESCE(telegram_chat_id,''), telegram_token_enc, telegram_enabled, telegram_delete_after_hours,
		event_types, updated_at
		FROM notification_channels WHERE org_id = ? AND mode = ?`, orgID, mode).
		Scan(&c.OrgID, &c.Mode, &c.Email, &emailOn, &c.TelegramChatID, &c.TelegramTokenEnc, &telegramOn,
			&c.TelegramDeleteAfterHours, &events, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationConfig{OrgID: orgID, Mode: mode, TelegramDeleteAfterHours: 47}, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(events), &c.EventTypes); err != nil {
		return c, err
	}
	c.EmailEnabled, c.TelegramEnabled, c.UpdatedAt = emailOn != 0, telegramOn != 0, fromMS(updated)
	return c, nil
}

func UpsertNotificationConfig(ctx context.Context, tx Tx, c NotificationConfig, now time.Time) error {
	events, err := json.Marshal(c.EventTypes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_channels
		(org_id, mode, email, email_enabled, telegram_chat_id, telegram_token_enc, telegram_enabled,
		 telegram_delete_after_hours, event_types, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, mode) DO UPDATE SET email = excluded.email, email_enabled = excluded.email_enabled,
		telegram_chat_id = excluded.telegram_chat_id, telegram_token_enc = excluded.telegram_token_enc,
		telegram_enabled = excluded.telegram_enabled, telegram_delete_after_hours = excluded.telegram_delete_after_hours,
		event_types = excluded.event_types, updated_at = excluded.updated_at`,
		c.OrgID, c.Mode, nullIfEmpty(c.Email), b2i(c.EmailEnabled), nullIfEmpty(c.TelegramChatID), c.TelegramTokenEnc,
		b2i(c.TelegramEnabled), c.TelegramDeleteAfterHours, string(events), ms(now))
	return err
}

// TelegramPaymentMessageKey names the Telegram message that shows a payment in
// one chat.
type TelegramPaymentMessageKey struct{ OrgID, Mode, ChatID, PaymentID string }

// LockTelegramPaymentMessage takes the payment's message lock until until and
// returns its known message id, 0 when none was posted yet. locked is false
// while another job holds an unexpired lock.
func LockTelegramPaymentMessage(ctx context.Context, tx Tx, k TelegramPaymentMessageKey, now, until time.Time) (messageID int64, locked bool, err error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO telegram_payment_messages
		(org_id, mode, chat_id, payment_id, locked_until, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, mode, chat_id, payment_id) DO UPDATE SET locked_until = excluded.locked_until
		WHERE telegram_payment_messages.locked_until <= ?`,
		k.OrgID, k.Mode, k.ChatID, k.PaymentID, ms(until), ms(now), ms(now))
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return 0, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT message_id FROM telegram_payment_messages
		WHERE org_id = ? AND mode = ? AND chat_id = ? AND payment_id = ?`, k.OrgID, k.Mode, k.ChatID, k.PaymentID).Scan(&messageID)
	return messageID, err == nil, err
}

// UnlockTelegramPaymentMessage records the message now showing the payment and
// releases the lock taken until until. A lock that expired and was taken over
// is left alone.
func UnlockTelegramPaymentMessage(ctx context.Context, tx Tx, k TelegramPaymentMessageKey, messageID int64, until time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE telegram_payment_messages SET message_id = ?, locked_until = 0
		WHERE org_id = ? AND mode = ? AND chat_id = ? AND payment_id = ? AND locked_until = ?`,
		messageID, k.OrgID, k.Mode, k.ChatID, k.PaymentID, ms(until))
	return err
}

// ForgetTelegramPaymentMessage drops the record of a deleted message. A message
// posted to replace it since is kept.
func ForgetTelegramPaymentMessage(ctx context.Context, tx Tx, k TelegramPaymentMessageKey, messageID int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM telegram_payment_messages
		WHERE org_id = ? AND mode = ? AND chat_id = ? AND payment_id = ? AND message_id = ?`,
		k.OrgID, k.Mode, k.ChatID, k.PaymentID, messageID)
	return err
}
