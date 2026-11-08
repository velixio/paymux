package outbound

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/auth"
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
	// TelegramLinkTTL is how long a link that connects a Telegram account works.
	TelegramLinkTTL = 10 * time.Minute

	// The buttons of a tap notification.
	signinTapApprove = "signin:approve"
	signinTapDeny    = "signin:deny"

	signinTickEvery = 2 * time.Second
	// signinClearRetryEvery is the pace once only a stuck notification is left, as
	// when the channel was turned off after it was sent.
	signinClearRetryEvery = 5 * time.Minute
	// signinNotificationStart begins every notification, so a reply to one whose
	// sending was not recorded is still known for what it is.
	signinNotificationStart = "Sign-in request\n"
	// signinWorkBudget ends a tick's work before its lease can run out.
	signinWorkBudget = 25 * time.Second
	// The bot may also sit in groups, whose messages can reach it, so a backlog can be long:
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

// IsSigninProblem reports whether err is written for the person, to be shown as is.
func IsSigninProblem(err error) bool {
	var p signinProblem
	return errors.As(err, &p)
}

const (
	errSigninChannelOff  = signinProblem("Turn on the live Telegram channel in Settings first.")
	errSigninUnreachable = signinProblem("Paymux could not reach the Telegram bot. Check the bot token in Settings.")
	errSigninWebhook     = signinProblem("This bot has a webhook set. Remove it so Paymux can read replies.")
)

// signinChannel loads the live Telegram channel and opens its token. Sign-in
// approval uses its bot only: messages go to each user's private chat.
func (s *Service) signinChannel(ctx context.Context, orgID string) (store.NotificationConfig, string, error) {
	var cfg store.NotificationConfig
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		cfg, err = store.GetNotificationConfig(ctx, q, orgID, signinMode)
		return err
	}); err != nil {
		return cfg, "", err
	}
	if !cfg.TelegramEnabled || len(cfg.TelegramTokenEnc) == 0 {
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

// CheckSigninApprovalChannel reports whether the live Telegram channel's bot can
// approve sign-ins. Its error is written for the person turning approval on.
func (s *Service) CheckSigninApprovalChannel(ctx context.Context, orgID string) error {
	_, err := s.signinBot(ctx, orgID)
	return err
}

// signinBot checks the bot and returns its username.
func (s *Service) signinBot(ctx context.Context, orgID string) (string, error) {
	_, token, err := s.signinChannel(ctx, orgID)
	if err != nil {
		return "", err
	}
	var hook struct {
		URL string `json:"url"`
	}
	if s.telegramResult(ctx, token, "getWebhookInfo", map[string]any{}, &hook) != nil {
		return "", errSigninUnreachable
	}
	if hook.URL != "" {
		return "", errSigninWebhook
	}
	var me struct {
		Username string `json:"username"`
	}
	if s.telegramResult(ctx, token, "getMe", map[string]any{}, &me) != nil || me.Username == "" {
		return "", errSigninUnreachable
	}
	return me.Username, nil
}

// BeginTelegramLink makes a one-time link that connects the user's Telegram
// account: opened in Telegram, it starts a private chat with the bot. It replaces
// the user's earlier link.
func (s *Service) BeginTelegramLink(ctx context.Context, orgID, userID string) (string, error) {
	username, err := s.signinBot(ctx, orgID)
	if err != nil {
		return "", err
	}
	code, hash := auth.NewSessionToken()
	now := s.now()
	if err := s.db.AddTelegramLink(ctx, orgID, userID, hash, now.Add(TelegramLinkTTL)); err != nil {
		return "", err
	}
	if err := s.db.Write(ctx, func(tx store.Tx) error {
		return s.q.Enqueue(ctx, tx, jobs.Job{Kind: JobSigninApprovalTick, Payload: struct{}{}, RunAt: now, DedupeKey: "signin_tick:link:" + hash})
	}); err != nil {
		return "", err
	}
	return "https://t.me/" + username + "?start=" + code, nil
}

// SigninApprovalInput starts a request. BrowserHash is the hash of the secret
// held by the browser that may collect the session.
type SigninApprovalInput struct {
	OrgID, UserID, BrowserHash, IP string
	Method                         string // store.SigninMethodTap or store.SigninMethodPIN
	Remember                       bool
}

// BeginSigninApproval stores a request and queues the work for it. The user's
// earlier pending request, if any, ends.
func (s *Service) BeginSigninApproval(ctx context.Context, in SigninApprovalInput) error {
	now, id := s.now(), ids.New("sia")
	return s.db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertSigninApproval(ctx, tx, store.SigninApproval{ID: id, OrgID: in.OrgID, UserID: in.UserID, BrowserHash: in.BrowserHash,
			IP: in.IP, Status: store.SigninPending, Method: in.Method, Remember: in.Remember, CreatedAt: now, ExpiresAt: now.Add(SigninApprovalTTL)}); err != nil {
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
		workCtx, cancel := context.WithTimeout(ctx, signinWorkBudget)
		if s.signinWorkCtx != nil {
			cancel()
			workCtx, cancel = s.signinWorkCtx(ctx)
		}
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
	open, err := s.db.HasOpenSigninApprovals(ctx, now)
	if err != nil || !open {
		return err
	}
	every := signinClearRetryEvery
	reading, err := s.db.SigninOrgsToRead(ctx, now)
	if err != nil {
		return err
	}
	if len(reading) > 0 {
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
	return s.db.PurgeSigninApprovals(ctx, now.Add(-signinPurgeAfter), now)
}

func (s *Service) sendSigninNotifications(ctx context.Context, now time.Time) error {
	due, err := s.db.SigninApprovalsToSend(ctx, now)
	if err != nil {
		return err
	}
	for _, a := range due {
		_, token, err := s.signinChannel(ctx, a.OrgID)
		if err != nil {
			s.log.Warn("sign-in approval: the live Telegram channel cannot be used", "request", a.ID, "err", err)
			continue
		}
		u, err := s.db.UserByID(ctx, a.UserID)
		if err != nil {
			return err
		}
		if u.TelegramUserID == 0 {
			// Unlinking ends the user's requests, so this one is ending too.
			continue
		}
		text := fmt.Sprintf(signinNotificationStart+"%s\nIP: %s\nTime: %s\n", u.Email, a.IP, a.CreatedAt.UTC().Format(time.RFC3339))
		var markup map[string]any
		if a.Method == store.SigninMethodPIN {
			text += "Reply to this message with your PIN."
			markup = map[string]any{"force_reply": true}
		} else {
			text += "Approve only if this is you."
			markup = map[string]any{"inline_keyboard": [][]map[string]string{{
				{"text": "Approve", "callback_data": signinTapApprove}, {"text": "Deny", "callback_data": signinTapDeny}}}}
		}
		var sent struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		}
		err = s.telegramResult(ctx, token, "sendMessage", map[string]any{"chat_id": u.TelegramUserID, "text": text,
			"reply_markup": markup}, &sent)
		if err == nil && (sent.MessageID == 0 || sent.Chat.ID == 0) {
			err = fmt.Errorf("telegram sendMessage: response lacks the message")
		}
		if err != nil {
			s.log.Warn("sign-in approval: notification not sent", "request", a.ID, "err", err)
			continue
		}
		// Saved even when time has just run out: the message is in the chat now.
		stopless := context.WithoutCancel(ctx)
		recorded, err := s.db.SetSigninNotification(stopless, a.ID, sent.Chat.ID, sent.MessageID)
		if err != nil {
			return err
		}
		if !recorded {
			if err := s.removeSigninMessage(stopless, a.OrgID, token, sent.Chat.ID, sent.MessageID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// readSigninReplies reads each bot that has a request or a link waiting. A bot's
// updates are read by one worker at a time, and only while something waits.
func (s *Service) readSigninReplies(ctx context.Context, now time.Time) error {
	orgs, err := s.db.SigninOrgsToRead(ctx, now)
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
		offset, readErr := s.readBotReplies(ctx, orgID, token, offset, now)
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

type signinChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type signinFrom struct {
	ID int64 `json:"id"`
}

// signinMessage is what this feature reads of a message. Its text is only ever
// compared.
type signinMessage struct {
	MessageID int64       `json:"message_id"`
	Chat      signinChat  `json:"chat"`
	From      *signinFrom `json:"from"`
	Text      string      `json:"text"`
	ReplyTo   *struct {
		MessageID int64       `json:"message_id"`
		From      *signinFrom `json:"from"`
		Text      string      `json:"text"`
	} `json:"reply_to_message"`
}

// signinUpdate is a message, or a tap on a notification's button.
type signinUpdate struct {
	UpdateID int64          `json:"update_id"`
	Message  *signinMessage `json:"message"`
	Callback *struct {
		ID      string         `json:"id"`
		From    signinFrom     `json:"from"`
		Message *signinMessage `json:"message"`
		Data    string         `json:"data"`
	} `json:"callback_query"`
}

// readBotReplies handles what is waiting, a bounded amount per tick, and returns
// the offset to continue from: past the last update handled, so a restart loses
// no reply.
func (s *Service) readBotReplies(ctx context.Context, orgID, token string, offset int64, now time.Time) (int64, error) {
	for range signinBatchesPerTick {
		status, body, err := s.telegramCallCapped(ctx, token, "getUpdates", map[string]any{"offset": offset, "timeout": 0,
			"limit": signinBatchSize, "allowed_updates": []string{"message", "callback_query"}}, signinBatchMaxBytes)
		var updates []signinUpdate
		if err == nil {
			err = decodeTelegramResult("getUpdates", status, body, &updates)
		}
		if err != nil {
			s.log.Warn("sign-in approval: replies not read", "err", err)
			return offset, nil
		}
		for _, u := range updates {
			if err := s.handleSigninUpdate(ctx, orgID, token, u, now); err != nil {
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

// handleSigninUpdate acts on taps and replies to this feature's notifications, and
// on links opened in a private chat. Everything else is left alone.
func (s *Service) handleSigninUpdate(ctx context.Context, orgID, token string, u signinUpdate, now time.Time) error {
	if u.Callback != nil {
		return s.handleSigninTap(ctx, orgID, token, u, now)
	}
	if u.Message == nil {
		return nil
	}
	if u.Message.ReplyTo == nil {
		return s.handleTelegramStart(ctx, orgID, token, u.Message, now)
	}
	return s.handleSigninReply(ctx, orgID, token, u.Message, now)
}

// botSent reports whether a message was sent by this bot as a notification.
func botSent(token string, from *signinFrom, text string) bool {
	botID, _, _ := strings.Cut(token, ":")
	return from != nil && strconv.FormatInt(from.ID, 10) == botID && strings.HasPrefix(text, signinNotificationStart)
}

// handleTelegramStart connects the sender's Telegram account when the message is
// "/start <code>" in a private chat and the code is a live link. The bot answers
// either way, so the person knows whether it worked.
func (s *Service) handleTelegramStart(ctx context.Context, orgID, token string, m *signinMessage, now time.Time) error {
	code, ok := strings.CutPrefix(m.Text, "/start ")
	if !ok || m.Chat.Type != "private" || m.From == nil || m.From.ID != m.Chat.ID {
		return nil
	}
	answer := "This link has expired or was already used. Make a new one in Paymux under Your sessions."
	u, err := s.db.UseTelegramLink(ctx, orgID, auth.HashToken(strings.TrimSpace(code)), m.From.ID, now)
	switch {
	case err == nil:
		answer = "Connected to Paymux as " + u.Email + ". You can now turn on sign-in approval in Paymux."
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	if err := s.telegramResult(ctx, token, "sendMessage", map[string]any{"chat_id": m.Chat.ID, "text": answer}, &struct{}{}); err != nil {
		s.log.Warn("sign-in approval: link answer not sent", "err", err)
	}
	return nil
}

// handleSigninTap applies Approve or Deny. Only a tap by the user, in their own
// private chat, on a notification for a tap request decides it.
func (s *Service) handleSigninTap(ctx context.Context, orgID, token string, u signinUpdate, now time.Time) error {
	cb := u.Callback
	answer := "This sign-in request has ended."
	defer func() {
		if err := s.telegramResult(context.WithoutCancel(ctx), token, "answerCallbackQuery",
			map[string]any{"callback_query_id": cb.ID, "text": answer}, new(bool)); err != nil {
			s.log.Warn("sign-in approval: tap not answered", "err", err)
		}
	}()
	if cb.Message == nil || (cb.Data != signinTapApprove && cb.Data != signinTapDeny) {
		return nil
	}
	chat := cb.Message.Chat.ID
	a, err := s.db.SigninApprovalByNotification(ctx, chat, cb.Message.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		// A notification whose sending was not recorded: it decides nothing and goes.
		if botSent(token, cb.Message.From, cb.Message.Text) {
			return s.removeSigninMessage(ctx, orgID, token, chat, cb.Message.MessageID, now)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if a.Method != store.SigninMethodTap || cb.From.ID != chat || a.Status != store.SigninPending || !a.ExpiresAt.After(now) {
		return nil
	}
	user, err := s.db.UserByID(ctx, a.UserID)
	if err != nil {
		return err
	}
	if user.TelegramUserID != cb.From.ID {
		return nil
	}
	a, err = s.db.DecideSigninApproval(ctx, a.ID, cb.Data == signinTapApprove, now)
	if err != nil {
		return err
	}
	switch {
	case a.Status == store.SigninApproved:
		answer = "Approved. Your browser will sign in."
	case cb.Data == signinTapDeny:
		answer = "Denied. That sign-in will not go ahead."
	}
	return nil
}

// handleSigninReply acts only on replies to this feature's notifications. Every
// such reply is deleted, right or wrong: a wrong one may be a mistyped PIN.
func (s *Service) handleSigninReply(ctx context.Context, orgID, token string, m *signinMessage, now time.Time) error {
	chat, to := m.Chat.ID, m.ReplyTo
	a, err := s.db.SigninApprovalByNotification(ctx, chat, to.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		// A notification this bot sent but whose sending was not recorded, as when
		// Telegram's answer was lost. The reply may hold a PIN: both go, and no
		// request is decided by it.
		if !botSent(token, to.From, to.Text) {
			return nil
		}
		if err := s.removeSigninMessage(ctx, orgID, token, chat, m.MessageID, now); err != nil {
			return err
		}
		return s.removeSigninMessage(ctx, orgID, token, chat, to.MessageID, now)
	}
	if err != nil {
		return err
	}
	if err := s.removeSigninMessage(ctx, orgID, token, chat, m.MessageID, now); err != nil {
		return err
	}
	if m.Text == "" || a.Method != store.SigninMethodPIN || a.Status != store.SigninPending || !a.ExpiresAt.After(now) {
		return nil
	}
	user, err := s.db.UserByID(ctx, a.UserID)
	if err != nil {
		return err
	}
	if m.From == nil || user.TelegramUserID != m.From.ID {
		// The chat is the user's own, so this only happens after a change of link.
		return nil
	}
	pin, err := s.box.Open(user.SigninPINEnc, secrets.AADSigninPIN(a.UserID))
	if err != nil {
		// Not the sender's fault, so not counted. The request runs out of time.
		s.log.Warn("sign-in approval: the PIN cannot be opened", "request", a.ID)
		return nil
	}
	correct := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(m.Text)), pin) == 1
	_, err = s.db.RecordSigninReply(ctx, a.ID, correct, SigninMaxWrongPINs, now)
	return err
}

// removeSigninMessage deletes a message that may hold a PIN, or keeps it to try
// again. Cut off by the time limit it returns the error instead, so the reply is
// read again by the next tick.
func (s *Service) removeSigninMessage(ctx context.Context, orgID, token string, chatID, messageID int64, now time.Time) error {
	if s.deleteSigninMessage(ctx, token, chatID, messageID) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.AddSigninDeletion(ctx, store.SigninDeletion{OrgID: orgID, ChatID: chatID, MessageID: messageID, CreatedAt: now})
}

// clearSigninNotifications deletes the notifications of decided requests and the
// messages whose deletion failed before. Past Telegram's deletion limit a failure
// stops being worth retrying.
func (s *Service) clearSigninNotifications(ctx context.Context, now time.Time) error {
	left, err := s.db.SigninDeletions(ctx)
	if err != nil {
		return err
	}
	for _, d := range left {
		gone := false
		if _, token, err := s.signinChannel(ctx, d.OrgID); err != nil {
			s.log.Warn("sign-in approval: the live Telegram channel cannot be used", "org", d.OrgID, "err", err)
		} else {
			gone = s.deleteSigninMessage(ctx, token, d.ChatID, d.MessageID)
		}
		if gone || now.Sub(d.CreatedAt) > signinDeleteLimit {
			if err := s.db.DropSigninDeletion(ctx, d.ChatID, d.MessageID); err != nil {
				return err
			}
		}
	}
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
