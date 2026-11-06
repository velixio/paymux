package outbound

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	// signinClearRetryEvery is the pace once only a stuck notification is left, as
	// when the channel was turned off after it was sent.
	signinClearRetryEvery = 5 * time.Minute
	// signinWorkBudget ends a tick's work before its lease can run out.
	signinWorkBudget = 25 * time.Second
	// A group's ordinary messages reach an admin bot too, so a backlog can be long:
	// up to signinBatchSize*signinBatchesPerTick updates a tick, each batch read
	// with room for the largest messages Telegram allows.
	signinBatchSize      = 100
	signinBatchesPerTick = 10
	signinBatchMaxBytes  = 8 << 20
	// signinLease is how long a crashed worker can hold up the others.
	signinLease = 30 * time.Second
	// signinTickLeaseKey is the signin_polls row of the tick itself. Bot ids are
	// numbers, so it cannot be one.
	signinTickLeaseKey = "tick"
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
	return decodeTelegramResult(method, status, body, result)
}

func decodeTelegramResult(method string, status int, body []byte, result any) error {
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
	// Ticks can be due together, here or on another instance. One does the work,
	// so nothing is sent or deleted twice; the others only keep the chain going.
	until := now.Add(signinLease)
	_, mine, err := s.db.ClaimSigninPoll(ctx, signinTickLeaseKey, now, until)
	if err != nil {
		return err
	}
	if mine {
		// The work stops before the lease can run out, so the next holder never
		// overlaps it. What is left is done by the next tick.
		workCtx, cancel := context.WithTimeout(ctx, s.signinBudget)
		err := s.signinApprovalWork(workCtx, now)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			s.log.Warn("sign-in approval: the tick ran out of time")
			err = nil
		}
		cancel()
		if releaseErr := s.db.ReleaseSigninPoll(context.WithoutCancel(ctx), signinTickLeaseKey, until, 0); err == nil {
			err = releaseErr
		}
		if err != nil {
			return err
		}
	}
	open, err := s.db.HasOpenSigninApprovals(ctx)
	if err != nil || !open {
		return err
	}
	every := signinClearRetryEvery
	pending, err := s.db.PendingSigninOrgs(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		every = signinTickEvery
	}
	// A running job keeps its own key taken, and several chains may be alive, so
	// the next run's key names its time slot: chains meeting there become one.
	next := now.Add(every)
	return s.db.Write(ctx, func(tx store.Tx) error {
		return s.q.Enqueue(ctx, tx, jobs.Job{Kind: JobSigninApprovalTick, Payload: struct{}{}, RunAt: next,
			DedupeKey: fmt.Sprintf("signin_tick:%d:%d", every.Milliseconds(), next.UnixMilli()/every.Milliseconds())})
	})
}

func (s *Service) signinApprovalWork(ctx context.Context, now time.Time) error {
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
	return s.db.PurgeSigninApprovals(ctx, now.Add(-signinPurgeAfter))
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
		// Saved even when time has just run out: the message is in the chat now.
		stopless := context.WithoutCancel(ctx)
		recorded, err := s.db.SetSigninNotification(stopless, a.ID, sent.Chat.ID, sent.MessageID)
		if err != nil {
			return err
		}
		if !recorded {
			s.deleteSigninMessage(stopless, token, sent.Chat.ID, sent.MessageID)
		}
	}
	return nil
}

// readSigninReplies reads each bot that has a request waiting. A bot's replies
// are read by one worker at a time, and only while a request is pending.
func (s *Service) readSigninReplies(ctx context.Context, now time.Time) error {
	orgs, err := s.db.PendingSigninOrgs(ctx)
	if err != nil {
		return err
	}
	for _, orgID := range orgs {
		_, token, err := s.signinChannel(ctx, orgID)
		if err != nil {
			s.log.Warn("sign-in approval: the live Telegram channel cannot be used", "org", orgID, "err", err)
			continue
		}
		botID, _, _ := strings.Cut(token, ":")
		until := now.Add(signinLease)
		offset, ok, err := s.db.ClaimSigninPoll(ctx, botID, now, until)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		offset, readErr := s.readBotReplies(ctx, token, offset, now)
		// Saved even when the job is being stopped: otherwise the claim stays taken
		// and handled replies are read again.
		if err := s.db.ReleaseSigninPoll(context.WithoutCancel(ctx), botID, until, offset); err != nil {
			return err
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

// signinUpdate is what a reply's update holds. Its text is only ever compared.
type signinUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text    string `json:"text"`
		ReplyTo *struct {
			MessageID int64 `json:"message_id"`
		} `json:"reply_to_message"`
	} `json:"message"`
}

// readBotReplies handles what is waiting, a bounded amount per tick, and returns
// the offset to continue from: past the last update handled, so a restart loses
// no reply.
func (s *Service) readBotReplies(ctx context.Context, token string, offset int64, now time.Time) (int64, error) {
	for range signinBatchesPerTick {
		status, body, err := s.telegramCallCapped(ctx, token, "getUpdates", map[string]any{"offset": offset, "timeout": 0,
			"limit": signinBatchSize, "allowed_updates": []string{"message"}}, signinBatchMaxBytes)
		var updates []signinUpdate
		if err == nil {
			err = decodeTelegramResult("getUpdates", status, body, &updates)
		}
		if err != nil {
			s.log.Warn("sign-in approval: replies not read", "err", err)
			return offset, nil
		}
		for _, u := range updates {
			if err := s.handleSigninUpdate(ctx, token, u, now); err != nil {
				return offset, err
			}
			offset = u.UpdateID + 1
		}
		if len(updates) < signinBatchSize {
			break
		}
	}
	return offset, nil
}

// handleSigninUpdate acts only on replies to this feature's notifications. Every
// such reply is deleted, right or wrong: a wrong one may be a mistyped PIN.
func (s *Service) handleSigninUpdate(ctx context.Context, token string, u signinUpdate, now time.Time) error {
	if u.Message == nil || u.Message.ReplyTo == nil {
		return nil
	}
	a, err := s.db.SigninApprovalByNotification(ctx, u.Message.Chat.ID, u.Message.ReplyTo.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	s.deleteSigninMessage(ctx, token, u.Message.Chat.ID, u.Message.MessageID)
	// Cut off by the time limit: the next tick reads this reply again.
	if err := ctx.Err(); err != nil {
		return err
	}
	if u.Message.Text == "" || a.Status != store.SigninPending || !a.ExpiresAt.After(now) {
		return nil
	}
	user, err := s.db.UserByID(ctx, a.UserID)
	if err != nil {
		return err
	}
	pin, err := s.box.Open(user.SigninPINEnc, secrets.AADSigninPIN(a.UserID))
	if err != nil {
		// Not the sender's fault, so not counted. The request runs out of time.
		s.log.Warn("sign-in approval: the PIN cannot be opened", "request", a.ID)
		return nil
	}
	correct := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(u.Message.Text)), pin) == 1
	_, err = s.db.RecordSigninReply(ctx, a.ID, correct, SigninMaxWrongPINs, now)
	return err
}

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
