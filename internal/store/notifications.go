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
	EventTypes                         []string
	UpdatedAt                          time.Time
}

func GetNotificationConfig(ctx context.Context, q Queryer, orgID, mode string) (NotificationConfig, error) {
	var c NotificationConfig
	var emailOn, telegramOn int
	var events string
	var updated int64
	err := q.QueryRowContext(ctx, `SELECT org_id, mode, COALESCE(email,''), email_enabled,
		COALESCE(telegram_chat_id,''), telegram_token_enc, telegram_enabled, event_types, updated_at
		FROM notification_channels WHERE org_id = ? AND mode = ?`, orgID, mode).
		Scan(&c.OrgID, &c.Mode, &c.Email, &emailOn, &c.TelegramChatID, &c.TelegramTokenEnc, &telegramOn, &events, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationConfig{OrgID: orgID, Mode: mode}, nil
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
		(org_id, mode, email, email_enabled, telegram_chat_id, telegram_token_enc, telegram_enabled, event_types, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (org_id, mode) DO UPDATE SET email = excluded.email, email_enabled = excluded.email_enabled,
		telegram_chat_id = excluded.telegram_chat_id, telegram_token_enc = excluded.telegram_token_enc,
		telegram_enabled = excluded.telegram_enabled, event_types = excluded.event_types, updated_at = excluded.updated_at`,
		c.OrgID, c.Mode, nullIfEmpty(c.Email), b2i(c.EmailEnabled), nullIfEmpty(c.TelegramChatID), c.TelegramTokenEnc,
		b2i(c.TelegramEnabled), string(events), ms(now))
	return err
}
