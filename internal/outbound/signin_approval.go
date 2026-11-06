package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

// Sign-in approval always uses the live channel, whatever mode the dashboard is
// showing: it protects the dashboard, not one mode's data.
const signinMode = "live"

var errSigninChannelOff = errors.New("Turn on the live Telegram channel in Settings first.")

// signinChannel loads the live Telegram channel and opens its token.
func (s *Service) signinChannel(ctx context.Context, orgID string) (store.NotificationConfig, string, error) {
	var cfg store.NotificationConfig
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		cfg, err = store.GetNotificationConfig(ctx, q, orgID, signinMode)
		return err
	}); err != nil {
		return cfg, "", err
	}
	if !cfg.TelegramEnabled || cfg.TelegramChatID == "" || len(cfg.TelegramTokenEnc) == 0 {
		return cfg, "", errSigninChannelOff
	}
	token, err := s.box.Open(cfg.TelegramTokenEnc, secrets.AADNotificationTelegram(orgID, signinMode))
	if err != nil {
		return cfg, "", err
	}
	return cfg, string(token), nil
}

// telegramResult makes a call and decodes its result. Errors hold the status
// only: a response body can quote what was sent to the chat.
func (s *Service) telegramResult(ctx context.Context, token, method string, payload map[string]any, result any) error {
	status, body, err := s.telegramCall(ctx, token, method, payload)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("telegram %s: http %d", method, status)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegram %s: unreadable response", method)
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("telegram %s: unreadable response", method)
	}
	return nil
}

// CheckSigninApprovalChannel reports whether the live Telegram channel can
// approve sign-ins. Its error is written for the person setting a PIN.
func (s *Service) CheckSigninApprovalChannel(ctx context.Context, orgID string) error {
	cfg, token, err := s.signinChannel(ctx, orgID)
	if err != nil {
		return err
	}
	unreachable := errors.New("Paymux could not reach the Telegram chat. Check the bot token and chat id in Settings.")
	var hook struct {
		URL string `json:"url"`
	}
	if s.telegramResult(ctx, token, "getWebhookInfo", map[string]any{}, &hook) != nil {
		return unreachable
	}
	if hook.URL != "" {
		return errors.New("This bot has a webhook set. Remove it so Paymux can read replies.")
	}
	var chat struct {
		Type string `json:"type"`
	}
	if s.telegramResult(ctx, token, "getChat", map[string]any{"chat_id": cfg.TelegramChatID}, &chat) != nil {
		return unreachable
	}
	switch chat.Type {
	case "private":
		return nil
	case "group", "supergroup":
	case "channel":
		return errors.New("Channels can't be used, because they have no normal replies. Use a private chat or a group.")
	default:
		return unreachable
	}
	var me struct {
		ID int64 `json:"id"`
	}
	if s.telegramResult(ctx, token, "getMe", map[string]any{}, &me) != nil {
		return unreachable
	}
	var member struct {
		Status    string `json:"status"`
		CanDelete bool   `json:"can_delete_messages"`
	}
	if s.telegramResult(ctx, token, "getChatMember", map[string]any{"chat_id": cfg.TelegramChatID, "user_id": me.ID}, &member) != nil {
		return unreachable
	}
	if member.Status != "administrator" || !member.CanDelete {
		return errors.New("Make the bot an admin of this group with permission to delete messages.")
	}
	return nil
}
