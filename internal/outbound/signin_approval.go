package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

const (
	// Sign-in approval always uses the live channel, whatever mode the dashboard
	// is showing: it protects the dashboard, not one mode's data.
	signinMode = "live"

	// JobSigninApprovalTick sends notifications, reads replies and cleans up. It
	// queues its own next run while anything is open.
	JobSigninApprovalTick = "signin_approval.tick"
	// SigninApprovalTTL is how long a request waits for a reply.
	SigninApprovalTTL = 5 * time.Minute
	// SigninMaxWrongPINs is how many wrong PINs end a request.
	SigninMaxWrongPINs = 5

	signinTickEvery = 2 * time.Second
	// signinPurgeAfter keeps ended requests long enough to recognise late replies:
	// Telegram holds updates for 24 hours.
	signinPurgeAfter = 48 * time.Hour
	// signinDeleteLimit is when Telegram no longer lets a bot delete a message.
	signinDeleteLimit = 47 * time.Hour
)

// signinProblem is an error written for the person setting a PIN.
type signinProblem string

func (p signinProblem) Error() string { return string(p) }

const (
	errSigninChannelOff  = signinProblem("Turn on the live Telegram channel in Settings first.")
	errSigninUnreachable = signinProblem("Paymux could not reach the Telegram chat. Check the bot token and chat id in Settings.")
	errSigninWebhook     = signinProblem("This bot has a webhook set. Remove it so Paymux can read replies.")
	errSigninIsChannel   = signinProblem("Channels can't be used, because they have no normal replies. Use a private chat or a group.")
	errSigninCantDelete  = signinProblem("Make the bot an admin of this group with permission to delete messages.")
)

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
	var hook struct {
		URL string `json:"url"`
	}
	if s.telegramResult(ctx, token, "getWebhookInfo", map[string]any{}, &hook) != nil {
		return errSigninUnreachable
	}
	if hook.URL != "" {
		return errSigninWebhook
	}
	var chat struct {
		Type string `json:"type"`
	}
	if s.telegramResult(ctx, token, "getChat", map[string]any{"chat_id": cfg.TelegramChatID}, &chat) != nil {
		return errSigninUnreachable
	}
	switch chat.Type {
	case "private":
		return nil
	case "group", "supergroup":
	case "channel":
		return errSigninIsChannel
	default:
		return errSigninUnreachable
	}
	var me struct {
		ID int64 `json:"id"`
	}
	if s.telegramResult(ctx, token, "getMe", map[string]any{}, &me) != nil {
		return errSigninUnreachable
	}
	var member struct {
		Status    string `json:"status"`
		CanDelete bool   `json:"can_delete_messages"`
	}
	if s.telegramResult(ctx, token, "getChatMember", map[string]any{"chat_id": cfg.TelegramChatID, "user_id": me.ID}, &member) != nil {
		return errSigninUnreachable
	}
	if member.Status != "administrator" || !member.CanDelete {
		return errSigninCantDelete
	}
	return nil
}

// SigninApprovalInput starts a request. BrowserHash is the hash of the secret
// held by the browser that may collect the session.
type SigninApprovalInput struct {
	OrgID, UserID, BrowserHash, IP string
	Remember                       bool
}

// BeginSigninApproval stores a request and queues the work for it. The user's
// earlier pending request, if any, ends.
func (s *Service) BeginSigninApproval(ctx context.Context, in SigninApprovalInput) error {
	now, id := s.now(), ids.New("sia")
	return s.db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertSigninApproval(ctx, tx, store.SigninApproval{ID: id, OrgID: in.OrgID, UserID: in.UserID, BrowserHash: in.BrowserHash,
			IP: in.IP, Status: store.SigninPending, Remember: in.Remember, CreatedAt: now, ExpiresAt: now.Add(SigninApprovalTTL)}); err != nil {
			return err
		}
		return s.q.Enqueue(ctx, tx, jobs.Job{Kind: JobSigninApprovalTick, Payload: struct{}{}, RunAt: now, DedupeKey: "signin_tick:req:" + id})
	})
}

// handleSigninApprovalTick does whatever is due. Only a database failure fails
// the job: Telegram failures are logged, and the next tick tries again, so a
// request stays pending until it runs out of time.
func (s *Service) handleSigninApprovalTick(ctx context.Context, _ json.RawMessage) error {
	now := s.now()
	if err := s.db.ExpireSigninApprovals(ctx, now); err != nil {
		return err
	}
	if err := s.sendSigninNotifications(ctx, now); err != nil {
		return err
	}
	if err := s.readSigninReplies(ctx, now); err != nil {
		return err
	}
	if err := s.clearSigninNotifications(ctx, now); err != nil {
		return err
	}
	if err := s.db.PurgeSigninApprovals(ctx, now.Add(-signinPurgeAfter)); err != nil {
		return err
	}
	open, err := s.db.HasOpenSigninApprovals(ctx)
	if err != nil || !open {
		return err
	}
	// A running job keeps its own key taken, and several chains may be alive, so
	// the next run's key names its time slot: chains meeting there become one.
	next := now.Add(signinTickEvery)
	return s.db.Write(ctx, func(tx store.Tx) error {
		return s.q.Enqueue(ctx, tx, jobs.Job{Kind: JobSigninApprovalTick, Payload: struct{}{}, RunAt: next,
			DedupeKey: fmt.Sprintf("signin_tick:%d", next.UnixMilli()/signinTickEvery.Milliseconds())})
	})
}

func (s *Service) sendSigninNotifications(ctx context.Context, now time.Time) error {
	due, err := s.db.SigninApprovalsToSend(ctx, now)
	if err != nil {
		return err
	}
	for _, a := range due {
		cfg, token, err := s.signinChannel(ctx, a.OrgID)
		if err != nil {
			s.log.Warn("sign-in approval: the live Telegram channel cannot be used", "request", a.ID, "err", err)
			continue
		}
		u, err := s.db.UserByID(ctx, a.UserID)
		if err != nil {
			return err
		}
		text := fmt.Sprintf("Sign-in request\n%s\nIP: %s\nTime: %s\nReply to this message with your PIN.",
			u.Email, a.IP, a.CreatedAt.UTC().Format(time.RFC3339))
		var sent struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		}
		err = s.telegramResult(ctx, token, "sendMessage", map[string]any{"chat_id": cfg.TelegramChatID, "text": text,
			"reply_markup": map[string]any{"force_reply": true}}, &sent)
		if err == nil && (sent.MessageID == 0 || sent.Chat.ID == 0) {
			err = fmt.Errorf("telegram sendMessage: response lacks the message")
		}
		if err != nil {
			s.log.Warn("sign-in approval: notification not sent", "request", a.ID, "err", err)
			continue
		}
		// Replies carry the chat's number, which the configured id may not be (it can
		// be @name), so the number Telegram answered with is what they match.
		if err := s.db.SetSigninNotification(ctx, a.ID, sent.Chat.ID, sent.MessageID); err != nil {
			return err
		}
	}
	return nil
}

// readSigninReplies is filled in by the next change.
func (s *Service) readSigninReplies(context.Context, time.Time) error { return nil }

// clearSigninNotifications deletes the notifications of decided requests. Past
// Telegram's deletion limit a failure stops being worth retrying.
func (s *Service) clearSigninNotifications(ctx context.Context, now time.Time) error {
	due, err := s.db.SigninApprovalsToClear(ctx)
	if err != nil {
		return err
	}
	for _, a := range due {
		gone := false
		if _, token, err := s.signinChannel(ctx, a.OrgID); err != nil {
			s.log.Warn("sign-in approval: the live Telegram channel cannot be used", "request", a.ID, "err", err)
		} else {
			gone = s.deleteSigninMessage(ctx, token, a.ChatID, a.MessageID)
		}
		if gone || now.Sub(a.CreatedAt) > signinDeleteLimit {
			if err := s.db.MarkSigninCleared(ctx, a.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteSigninMessage reports whether the message is gone. A failure logs ids
// only.
func (s *Service) deleteSigninMessage(ctx context.Context, token string, chatID, messageID int64) bool {
	status, body, err := s.telegramCall(ctx, token, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID})
	if err == nil && (status/100 == 2 || status == http.StatusBadRequest && bytes.Contains(body, []byte("message to delete not found"))) {
		return true
	}
	if err != nil {
		s.log.Warn("sign-in approval: message not deleted", "chat", chatID, "message", messageID, "err", err)
	} else {
		s.log.Warn("sign-in approval: message not deleted", "chat", chatID, "message", messageID, "status", status)
	}
	return false
}
