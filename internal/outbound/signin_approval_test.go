package outbound_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/outbound/telegramtest"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const signinBotToken = "123456:ABC_def-789"

// syncBuffer is a log sink the job workers can write to while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type signinHarness struct {
	*env
	t    *testing.T
	tg   *telegramtest.Fake
	svc  *outbound.Service
	q    *jobs.Queue
	logs *syncBuffer
}

func newSigninHarness(t *testing.T, db *store.DB) *signinHarness {
	t.Helper()
	e := newEnv(t, db, true)
	tg, logs := telegramtest.New(), &syncBuffer{}
	q := jobs.New(db, jobs.Options{Clock: e.clk.Now, Logger: slog.New(slog.DiscardHandler)})
	svc := outbound.New(outbound.Options{DB: db, Queue: q, Box: e.box, Now: e.clk.Now, Logger: slog.New(slog.NewTextHandler(logs, nil)),
		NotificationClient: &http.Client{Transport: tg}})
	svc.Register()
	h := &signinHarness{env: e, t: t, tg: tg, svc: svc, q: q, logs: logs}
	h.channel("-100123", true, signinBotToken)
	return h
}

func (h *signinHarness) channel(chat string, enabled bool, token string) {
	h.t.Helper()
	cfg := outbound.NotificationConfig{TelegramChatID: chat, TelegramEnabled: enabled}
	if err := h.svc.SaveNotificationConfig(ctx, "org_1", "live", cfg, token); err != nil {
		h.t.Fatal(err)
	}
}

func TestCheckSigninApprovalChannel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *signinHarness)
		want  string
	}{
		{"channel off", func(h *signinHarness) { h.channel("-100123", false, "") }, "Turn on the live Telegram channel in Settings first."},
		{"webhook set", func(h *signinHarness) { h.tg.SetWebhook("https://example.com/hook") }, "This bot has a webhook set. Remove it so Paymux can read replies."},
		{"a channel", func(h *signinHarness) { h.tg.SetChat(-100123, "channel") }, "Channels can't be used, because they have no normal replies. Use a private chat or a group."},
		{"group without delete rights", func(h *signinHarness) { h.tg.SetBotCanDelete(false) }, "Make the bot an admin of this group with permission to delete messages."},
		{"bad token", func(h *signinHarness) { h.tg.Fail("getChat", http.StatusUnauthorized) }, "Paymux could not reach the Telegram chat. Check the bot token and chat id in Settings."},
		{"private chat", func(h *signinHarness) { h.tg.SetChat(42, "private"); h.tg.SetBotCanDelete(false) }, ""},
		{"group with delete rights", func(h *signinHarness) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				h := newSigninHarness(t, db)
				tc.setup(h)
				err := h.svc.CheckSigninApprovalChannel(ctx, "org_1")
				got := ""
				if err != nil {
					got = err.Error()
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
				if strings.Contains(got, signinBotToken) {
					t.Fatal("the error holds the token")
				}
			})
		})
	}
}

func TestCheckSigninApprovalChannelNoConfigAtAll(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		if err := h.svc.CheckSigninApprovalChannel(ctx, "org_without_channel"); err == nil || err.Error() != "Turn on the live Telegram channel in Settings first." {
			t.Fatalf("got %v", err)
		}
	})
}
