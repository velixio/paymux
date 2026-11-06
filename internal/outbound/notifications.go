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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector/money"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

type notificationPayload struct {
	EventID string `json:"event_id"`
}

var telegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// DefaultTelegramDeleteAfterHours stays below Telegram's strict 48-hour deletion limit.
const DefaultTelegramDeleteAfterHours = 47

// MaxTelegramDeleteAfterHours is the largest retention Telegram can reliably support.
const MaxTelegramDeleteAfterHours = 47

// NotificationConfig is safe to render: the Telegram token is never returned.
type NotificationConfig struct {
	Email, TelegramChatID                           string
	EmailEnabled, TelegramEnabled, HasTelegramToken bool
	TelegramDeleteAfterHours                        int
	EventTypes                                      []string
}

func (s *Service) NotificationConfig(ctx context.Context, orgID, mode string) (NotificationConfig, error) {
	var row store.NotificationConfig
	err := s.db.Read(ctx, func(q store.Querier) (err error) { row, err = store.GetNotificationConfig(ctx, q, orgID, mode); return })
	return NotificationConfig{Email: row.Email, TelegramChatID: row.TelegramChatID, EmailEnabled: row.EmailEnabled,
		TelegramEnabled: row.TelegramEnabled, HasTelegramToken: len(row.TelegramTokenEnc) > 0,
		TelegramDeleteAfterHours: row.TelegramDeleteAfterHours, EventTypes: row.EventTypes}, err
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
	if c.TelegramDeleteAfterHours < 0 || c.TelegramDeleteAfterHours > MaxTelegramDeleteAfterHours {
		return fmt.Errorf("telegram message retention must be 0 (never) or 1–%d hours", MaxTelegramDeleteAfterHours)
	}
	c.EventTypes = uniqueEventTypes(c.EventTypes)
	return s.db.Write(ctx, func(tx store.Tx) error {
		return store.UpsertNotificationConfig(ctx, tx, store.NotificationConfig{OrgID: orgID, Mode: mode, Email: strings.TrimSpace(c.Email),
			EmailEnabled: c.EmailEnabled, TelegramChatID: strings.TrimSpace(c.TelegramChatID), TelegramTokenEnc: enc,
			TelegramEnabled: c.TelegramEnabled, TelegramDeleteAfterHours: c.TelegramDeleteAfterHours, EventTypes: c.EventTypes}, s.now())
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
	text := fmt.Sprintf("[%s] %s · %s", strings.ToUpper(ev.Mode), ev.Type, ev.ObjectID)
	first, second := s.notificationDetails(ctx, ev)
	text += first
	if second != "" {
		text += "\n" + second
	}
	// Lock before anything is sent: a job that has to wait is retried, and must
	// not have sent its email by then.
	var lock *telegramPaymentLock
	if cfg.TelegramEnabled && updatesTelegramPaymentMessage(ev) {
		lock, err = s.lockTelegramPaymentMessage(ctx, ev, cfg.TelegramChatID)
		if err != nil {
			return err
		}
		defer s.unlockTelegramPaymentMessage(ctx, lock)
	}
	var errs []error
	if cfg.EmailEnabled {
		if err := s.mailer.Send(ctx, notify.Message{To: []string{cfg.Email}, Subject: "[Paymux] " + ev.Type, Text: text + "\n"}); err != nil {
			errs = append(errs, err)
		}
	}
	if cfg.TelegramEnabled {
		if err := s.deliverTelegram(ctx, ev, cfg, text, lock); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// deliverTelegram posts text, or, with a lock, updates the payment's message.
// A message Telegram refuses to edit, because it was deleted or is from another
// bot, is posted again.
func (s *Service) deliverTelegram(ctx context.Context, ev store.EventRow, cfg store.NotificationConfig, text string, lock *telegramPaymentLock) error {
	token, err := s.box.Open(cfg.TelegramTokenEnc, secrets.AADNotificationTelegram(ev.OrgID, ev.Mode))
	if err != nil {
		return err
	}
	paymentID := ""
	if lock != nil {
		rendered, ok, err := s.telegramPaymentText(ctx, ev)
		if err != nil {
			return err
		}
		if ok {
			text, paymentID = rendered, ev.ObjectID
			if lock.messageID != 0 {
				status, response, err := s.telegramCall(ctx, string(token), "editMessageText",
					map[string]any{"chat_id": cfg.TelegramChatID, "message_id": lock.messageID, "text": text})
				switch {
				case err != nil:
					return err
				case status/100 == 2, status == http.StatusBadRequest && bytes.Contains(response, []byte("message is not modified")):
					return nil
				case status != http.StatusBadRequest:
					return fmt.Errorf("telegram http %d", status)
				}
			}
		}
	}
	status, response, err := s.telegramCall(ctx, string(token), "sendMessage", map[string]any{"chat_id": cfg.TelegramChatID, "text": text})
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("telegram http %d", status)
	}
	if paymentID == "" && cfg.TelegramDeleteAfterHours == 0 {
		return nil
	}
	var sent struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if json.Unmarshal(response, &sent) != nil || sent.Result.MessageID == 0 {
		s.log.Warn("telegram response lacked message id; the message cannot be updated or deleted later", "event_id", ev.ID)
		return nil
	}
	if paymentID != "" {
		lock.messageID = sent.Result.MessageID
	}
	if cfg.TelegramDeleteAfterHours == 0 {
		return nil
	}
	return s.scheduleTelegramDelete(ctx, ev, cfg, sent.Result.MessageID, paymentID)
}

// telegramCall makes one Bot API call. Errors never carry the URL, which holds
// the token.
func (s *Service) telegramCall(ctx context.Context, token, method string, payload map[string]any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("telegram request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.notifyClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return 0, nil, fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, response, nil
}

type telegramDeletePayload struct {
	OrgID     string `json:"org_id"`
	Mode      string `json:"mode"`
	ChatID    string `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	// PaymentID is set when the message is a payment's one message.
	PaymentID string `json:"payment_id,omitempty"`
}

func (s *Service) scheduleTelegramDelete(ctx context.Context, ev store.EventRow, cfg store.NotificationConfig, messageID int64, paymentID string) error {
	p := telegramDeletePayload{OrgID: ev.OrgID, Mode: ev.Mode, ChatID: cfg.TelegramChatID, MessageID: messageID, PaymentID: paymentID}
	return s.db.Write(ctx, func(tx store.Tx) error {
		return s.q.Enqueue(ctx, tx, jobs.Job{Kind: JobDeleteTelegram, Payload: p,
			RunAt:     s.now().Add(time.Duration(cfg.TelegramDeleteAfterHours) * time.Hour),
			DedupeKey: JobDeleteTelegram + ":" + ev.ID})
	})
}

func (s *Service) handleTelegramDelete(ctx context.Context, raw json.RawMessage) error {
	var p telegramDeletePayload
	if json.Unmarshal(raw, &p) != nil || p.OrgID == "" || p.Mode == "" || p.ChatID == "" || p.MessageID <= 0 {
		return jobs.Permanent(errors.New("bad Telegram deletion payload"))
	}
	var cfg store.NotificationConfig
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		cfg, err = store.GetNotificationConfig(ctx, q, p.OrgID, p.Mode)
		return
	}); err != nil {
		return err
	}
	token, err := s.box.Open(cfg.TelegramTokenEnc, secrets.AADNotificationTelegram(p.OrgID, p.Mode))
	if err != nil {
		return jobs.Permanent(err)
	}
	body, _ := json.Marshal(map[string]any{"chat_id": p.ChatID, "message_id": p.MessageID})
	target := "https://api.telegram.org/bot" + string(token) + "/deleteMessage"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.notifyClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram delete request failed: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		if p.PaymentID == "" {
			return nil
		}
		return s.db.Write(ctx, func(tx store.Tx) error {
			return store.ForgetTelegramPaymentMessage(ctx, tx,
				store.TelegramPaymentMessageKey{OrgID: p.OrgID, Mode: p.Mode, ChatID: p.ChatID, PaymentID: p.PaymentID}, p.MessageID)
		})
	}
	err = fmt.Errorf("telegram delete http %d", resp.StatusCode)
	if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests {
		return jobs.Permanent(err)
	}
	return err
}

// notificationPayment loads the event's payment and customer. ok is false when
// the payment is missing or belongs to another organization or mode.
func (s *Service) notificationPayment(ctx context.Context, ev store.EventRow) (payment store.Payment, customer store.Customer, ok bool, err error) {
	if ev.ObjectType != "payment" {
		return payment, customer, false, nil
	}
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		payment, err = store.GetPayment(ctx, q, ev.ObjectID)
		if err != nil || payment.OrgID != ev.OrgID || payment.Mode != ev.Mode {
			return err
		}
		if payment.CustomerID != "" {
			loaded, customerErr := store.GetCustomer(ctx, q, payment.CustomerID)
			switch {
			case customerErr == nil && loaded.OrgID == payment.OrgID && loaded.AppID == payment.AppID && loaded.Mode == payment.Mode:
				customer = loaded
			case errors.Is(customerErr, store.ErrNotFound):
				return nil
			case customerErr != nil:
				return customerErr
			}
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return payment, customer, false, nil
	}
	if err != nil {
		return payment, customer, false, err
	}
	return payment, customer, payment.OrgID == ev.OrgID && payment.Mode == ev.Mode, nil
}

func notificationAmount(p store.Payment) string {
	amount, err := money.FormatDecimal(p.Amount, p.Currency)
	if err != nil {
		amount = strconv.FormatInt(p.Amount, 10)
	}
	return amount + " " + p.Currency
}

// notificationCountry returns the payment's valid two-letter country, if any.
func notificationCountry(p store.Payment, c store.Customer) string {
	code := c.Billing.Country
	if code == "" {
		code = p.Country
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	if _, err := language.ParseRegion(code); err != nil {
		return ""
	}
	return code
}

func (s *Service) notificationDetails(ctx context.Context, ev store.EventRow) (string, string) {
	payment, customer, ok, err := s.notificationPayment(ctx, ev)
	if err != nil || !ok {
		return "", ""
	}
	var second []string
	if label := countryLabel(notificationCountry(payment, customer)); label != "" {
		second = append(second, label)
	}
	if customer.Email != "" {
		second = append(second, customer.Email)
	}
	return " · " + notificationAmount(payment), strings.Join(second, " · ")
}

func countryFlag(code string) string {
	return string([]rune{rune(0x1F1E6) + rune(code[0]-'A'), rune(0x1F1E6) + rune(code[1]-'A')})
}

// countryLabel names a code from notificationCountry; "" stays "".
func countryLabel(code string) string {
	if code == "" {
		return ""
	}
	region, err := language.ParseRegion(code)
	if err != nil {
		return ""
	}
	name := display.English.Regions().Name(region)
	if name == "" || strings.EqualFold(name, code) {
		return countryFlag(code) + " " + code
	}
	return countryFlag(code) + " " + name + " (" + code + ")"
}
