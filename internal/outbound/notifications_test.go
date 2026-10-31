package outbound_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

type notificationMailer struct{ messages []notify.Message }

func (m *notificationMailer) Send(_ context.Context, msg notify.Message) error {
	m.messages = append(m.messages, msg)
	return nil
}

type notificationRoundTrip func(*http.Request) (*http.Response, error)

func (f notificationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOperatorNotificationsAreModeScopedAndSecretsStayEncrypted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		mailer := &notificationMailer{}
		var telegramPath string
		var telegramBody []byte
		client := &http.Client{Transport: notificationRoundTrip(func(r *http.Request) (*http.Response, error) {
			telegramPath = r.URL.Path
			telegramBody, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
		})}
		q := jobs.New(db, jobs.Options{Clock: e.clk.Now, Logger: slog.New(slog.DiscardHandler)})
		svc := outbound.New(outbound.Options{DB: db, Queue: q, Box: e.box, Now: e.clk.Now, Mailer: mailer, NotificationClient: client})
		svc.Register()

		cfg := outbound.NotificationConfig{Email: "ops@example.com", EmailEnabled: true, TelegramChatID: "-100123",
			TelegramEnabled: true, EventTypes: []string{"payment.succeeded", "payment.succeeded", "dispute.created"}}
		const token = "123456:ABC_def-789"
		if err := svc.SaveNotificationConfig(ctx, "org_1", "test", cfg, token); err != nil {
			t.Fatal(err)
		}
		var stored store.NotificationConfig
		if err := db.Read(ctx, func(q store.Querier) (err error) {
			stored, err = store.GetNotificationConfig(ctx, q, "org_1", "test")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(stored.TelegramTokenEnc), token) || len(stored.EventTypes) != 2 {
			t.Fatalf("unsafe notification config: %+v", stored)
		}
		plain, err := e.box.Open(stored.TelegramTokenEnc, secrets.AADNotificationTelegram("org_1", "test"))
		if err != nil || string(plain) != token {
			t.Fatalf("encrypted token: %q %v", plain, err)
		}

		if err := db.Write(ctx, func(tx store.Tx) error {
			return svc.Emit(ctx, tx, events.Event{OrgID: "org_1", AppID: "app_1", Mode: "test", Type: "payment.succeeded",
				ObjectType: "payment", ObjectID: "p123", ObjectVersion: 2, Data: map[string]any{"id": "p123"}})
		}); err != nil {
			t.Fatal(err)
		}
		ran, err := q.RunOnce(ctx)
		if err != nil || ran != 1 {
			t.Fatalf("notification jobs run = %d: %v", ran, err)
		}
		if len(mailer.messages) != 1 || mailer.messages[0].To[0] != "ops@example.com" || !strings.Contains(mailer.messages[0].Text, "p123") {
			t.Fatalf("email messages: %+v", mailer.messages)
		}
		if telegramPath != "/bot"+token+"/sendMessage" {
			t.Fatalf("telegram path %q", telegramPath)
		}
		var sent map[string]string
		if err := json.Unmarshal(telegramBody, &sent); err != nil || sent["chat_id"] != "-100123" || !strings.Contains(sent["text"], "p123") {
			t.Fatalf("telegram body %s: %v", telegramBody, err)
		}
	})
}

func TestNotificationConfigValidation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		for name, cfg := range map[string]struct {
			view  outbound.NotificationConfig
			token string
		}{
			"bad mode":       {outbound.NotificationConfig{}, ""},
			"email missing":  {outbound.NotificationConfig{EmailEnabled: true}, ""},
			"telegram token": {outbound.NotificationConfig{TelegramEnabled: true, TelegramChatID: "1"}, "not-a-token"},
		} {
			mode := "test"
			if name == "bad mode" {
				mode = "staging"
			}
			if err := e.svc.SaveNotificationConfig(ctx, "org_1", mode, cfg.view, cfg.token); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})
}
