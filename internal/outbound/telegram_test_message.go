package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

// TelegramTestScenario is a sample message an operator can send to the chat.
type TelegramTestScenario struct{ Key, Label string }

// TelegramTestScenarios lists the samples, in the order the form shows them.
var TelegramTestScenarios = []TelegramTestScenario{
	{"created", "Awaiting payment"},
	{"failed_once", "Failed once"},
	{"failed_many", "Failed several times"},
	{"paid", "Paid first try"},
	{"paid_after_failures", "Paid after failures"},
	{"canceled", "Canceled"},
	{"expired", "Expired"},
	{"alert", "Refund or dispute alert"},
}

// SendTelegramTest posts a sample message with made-up data to the saved chat.
// The message goes out even with Telegram notifications off, so a setup can be
// tried first. Errors are safe to show: they never carry the bot token.
func (s *Service) SendTelegramTest(ctx context.Context, orgID, mode, scenario string, sound bool) error {
	body, err := telegramTestText(mode, scenario, s.now())
	if err != nil {
		return err
	}
	var cfg store.NotificationConfig
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		cfg, err = store.GetNotificationConfig(ctx, q, orgID, mode)
		return err
	}); err != nil {
		return err
	}
	if len(cfg.TelegramTokenEnc) == 0 || cfg.TelegramChatID == "" {
		return errors.New("save a Telegram bot token and chat ID before sending a test message")
	}
	token, err := s.box.Open(cfg.TelegramTokenEnc, secrets.AADNotificationTelegram(orgID, mode))
	if err != nil {
		return err
	}
	payload := map[string]any{"chat_id": cfg.TelegramChatID, "text": "🧪 Test message\n" + body}
	if !sound {
		payload["disable_notification"] = true
	}
	status, response, err := s.telegramCall(ctx, string(token), "sendMessage", payload)
	if err != nil {
		return err
	}
	var reply struct {
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(response, &reply)
	if status/100 != 2 {
		if reply.Description != "" {
			return fmt.Errorf("telegram rejected the test message: %s", reply.Description)
		}
		return fmt.Errorf("telegram rejected the test message: http %d", status)
	}
	if cfg.TelegramDeleteAfterHours == 0 || reply.Result.MessageID == 0 {
		return nil
	}
	ev := store.EventRow{ID: "test_message_" + strconv.FormatInt(reply.Result.MessageID, 10), OrgID: orgID, Mode: mode}
	return s.scheduleTelegramDelete(ctx, ev, cfg, reply.Result.MessageID, "")
}

func telegramTestText(mode, scenario string, now time.Time) (string, error) {
	p := store.Payment{ID: "pay_test_message", Mode: mode, Amount: 9900, Currency: "USD", Status: "created"}
	if scenario == "alert" {
		return "[" + strings.ToUpper(mode) + "] refund.succeeded · re_test_message · " + notificationAmount(p), nil
	}
	failed := func(minutesAgo int, reason string) store.Attempt {
		return store.Attempt{Status: "failed", FailureMessage: reason, UpdatedAt: now.Add(-time.Duration(minutesAgo) * time.Minute)}
	}
	history := []store.Attempt{failed(7, "Insufficient funds"), failed(6, "Insufficient funds"), failed(5, "Card expired"),
		failed(4, "Card expired"), failed(3, "Do not honor"), failed(2, "3-D Secure failed"), failed(1, "Insufficient funds")}
	captured := store.Attempt{Status: "captured", UpdatedAt: now}
	var attempts []store.Attempt
	switch scenario {
	case "created":
	case "failed_once":
		p.Status, attempts = "failed", []store.Attempt{failed(1, "Insufficient funds")}
	case "failed_many":
		p.Status, attempts = "failed", history
	case "paid":
		p.Status, attempts = "succeeded", []store.Attempt{captured}
	case "paid_after_failures":
		p.Status, attempts = "succeeded", append(history, captured)
	case "canceled", "expired":
		p.Status = scenario
	default:
		return "", errors.New("choose a test message")
	}
	c := store.Customer{Email: "buyer@example.com", Billing: store.Billing{Country: "US"}}
	return renderTelegramPayment(store.EventRow{Mode: mode, CreatedAt: now}, p, c, attempts, nil), nil
}
