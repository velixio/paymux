package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

type notificationPayload struct {
	EventID string `json:"event_id"`
}

var telegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// NotificationConfig is safe to render: the Telegram token is never returned.
type NotificationConfig struct {
	Email, TelegramChatID                           string
	EmailEnabled, TelegramEnabled, HasTelegramToken bool
	EventTypes                                      []string
}

func (s *Service) NotificationConfig(ctx context.Context, orgID, mode string) (NotificationConfig, error) {
	var row store.NotificationConfig
	err := s.db.Read(ctx, func(q store.Querier) (err error) { row, err = store.GetNotificationConfig(ctx, q, orgID, mode); return })
	return NotificationConfig{Email: row.Email, TelegramChatID: row.TelegramChatID, EmailEnabled: row.EmailEnabled,
		TelegramEnabled: row.TelegramEnabled, HasTelegramToken: len(row.TelegramTokenEnc) > 0, EventTypes: row.EventTypes}, err
}

// SaveNotificationConfig validates and encrypts a replacement configuration.
// A blank token preserves the stored token; Telegram can be disabled without
// deleting it.
func (s *Service) SaveNotificationConfig(ctx context.Context, orgID, mode string, c NotificationConfig, token string) error {
	if mode != "test" && mode != "live" {
		return errors.New("notification mode must be test or live")
	}
	if c.Email != "" {
		if _, err := mail.ParseAddress(c.Email); err != nil || strings.ContainsAny(c.Email, "\r\n") {
			return errors.New("notification email is invalid")
		}
	}
	if c.EmailEnabled && c.Email == "" {
		return errors.New("enter an email address before enabling email notifications")
	}
	var old store.NotificationConfig
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { old, err = store.GetNotificationConfig(ctx, q, orgID, mode); return }); err != nil {
		return err
	}
	enc := old.TelegramTokenEnc
	if token = strings.TrimSpace(token); token != "" {
		if !telegramTokenPattern.MatchString(token) {
			return errors.New("telegram bot token is invalid")
		}
		var err error
		enc, err = s.box.Seal([]byte(token), secrets.AADNotificationTelegram(orgID, mode))
		if err != nil {
			return err
		}
	}
	if c.TelegramEnabled && (c.TelegramChatID == "" || len(enc) == 0) {
		return errors.New("enter a Telegram bot token and chat id before enabling Telegram notifications")
	}
	c.EventTypes = uniqueEventTypes(c.EventTypes)
	return s.db.Write(ctx, func(tx store.Tx) error {
		return store.UpsertNotificationConfig(ctx, tx, store.NotificationConfig{OrgID: orgID, Mode: mode, Email: strings.TrimSpace(c.Email),
			EmailEnabled: c.EmailEnabled, TelegramChatID: strings.TrimSpace(c.TelegramChatID), TelegramTokenEnc: enc,
			TelegramEnabled: c.TelegramEnabled, EventTypes: c.EventTypes}, s.now())
	})
}

func uniqueEventTypes(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, typ := range in {
		typ = strings.TrimSpace(typ)
		if typ != "" && !seen[typ] {
			seen[typ] = true
			out = append(out, typ)
		}
	}
	return out
}

func (s *Service) handleNotification(ctx context.Context, raw json.RawMessage) error {
	var p notificationPayload
	if json.Unmarshal(raw, &p) != nil || p.EventID == "" {
		return jobs.Permanent(errors.New("bad notification payload"))
	}
	var ev store.EventRow
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		ev, err = store.GetEventByID(ctx, q, p.EventID)
		return err
	})
	if err != nil {
		return err
	}
	var cfg store.NotificationConfig
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		cfg, err = store.GetNotificationConfig(ctx, q, ev.OrgID, ev.Mode)
		return
	}); err != nil {
		return err
	}
	if !matches(cfg.EventTypes, ev.Type) {
		return nil
	}
	text := fmt.Sprintf("[%s] %s: %s %s", strings.ToUpper(ev.Mode), ev.Type, ev.ObjectType, ev.ObjectID)
	var errs []error
	if cfg.EmailEnabled {
		if err := s.mailer.Send(ctx, notify.Message{To: []string{cfg.Email}, Subject: "[Paymux] " + ev.Type, Text: text + "\n"}); err != nil {
			errs = append(errs, err)
		}
	}
	if cfg.TelegramEnabled {
		token, err := s.box.Open(cfg.TelegramTokenEnc, secrets.AADNotificationTelegram(ev.OrgID, ev.Mode))
		if err != nil {
			errs = append(errs, err)
		} else {
			body, _ := json.Marshal(map[string]string{"chat_id": cfg.TelegramChatID, "text": text})
			target := "https://api.telegram.org/bot" + string(token) + "/sendMessage"
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := s.notifyClient.Do(req)
			if err != nil {
				errs = append(errs, err)
			} else {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				if resp.StatusCode/100 != 2 {
					errs = append(errs, fmt.Errorf("telegram http %d", resp.StatusCode))
				}
			}
		}
	}
	return errors.Join(errs...)
}
