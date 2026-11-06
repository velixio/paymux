package outbound_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func (h *telegramHarness) setSound(retentionHours int, sound ...string) {
	h.t.Helper()
	cfg := outbound.NotificationConfig{TelegramChatID: "-100123", TelegramEnabled: true, TelegramDeleteAfterHours: retentionHours,
		EventTypes: append(slices.Clone(lifecycleEvents), "refund.succeeded"), TelegramSoundEvents: sound}
	if err := h.svc.SaveNotificationConfig(ctx, "org_1", "test", cfg, ""); err != nil {
		h.t.Fatal(err)
	}
}

func TestTelegramSoundOnFirstMessageFollowsTheEvent(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.setSound(0, "payment.created")
		wantCalls(t, h.deliver("payment.created", "created"), telegramCall{Method: "sendMessage", Text: "⏳ " + telegramHead})
	})
}

func TestTelegramSilentLaterEventEditsAndSoundEventRepostsAtTheBottom(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.setSound(0, "payment.succeeded")
		wantCalls(t, h.deliver("payment.created", "created"), telegramCall{Method: "sendMessage", Silent: true, Text: "⏳ " + telegramHead})
		wantCalls(t, h.deliver("payment.failed", "failed"),
			telegramCall{Method: "editMessageText", MessageID: 321, Text: "❌ " + telegramHead + "\nFailed"})

		h.attempt("a1", "captured", "", "", at(16, 9))
		wantCalls(t, h.deliver("payment.succeeded", "succeeded"),
			telegramCall{Method: "sendMessage", Text: "✅ " + telegramHead + "\nPaid 16:09 UTC"},
			telegramCall{Method: "deleteMessage", MessageID: 321})

		// Later updates follow the new message, not the deleted one.
		wantCalls(t, h.deliver("payment.canceled", "canceled"),
			telegramCall{Method: "editMessageText", MessageID: 322, Text: "🚫 " + telegramHead + "\nCanceled"})
	})
}

// The new message is already out, so a retry would only ping everyone again.
func TestTelegramOldMessageThatCannotBeDeletedDoesNotFailTheJob(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.setSound(0, "payment.succeeded")
		h.deliver("payment.created", "created")
		h.tg.override = func(method string) (int, string, bool) {
			if method == "deleteMessage" {
				return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message can't be deleted"}`, true
			}
			return 0, "", false
		}
		if calls := h.deliver("payment.succeeded", "succeeded"); len(calls) != 2 {
			t.Fatalf("calls: %+v", calls)
		}
		h.e.clk.Add(time.Hour)
		h.run(0)
	})
}

// A sound re-post removes a message whose retention delete is still queued.
func TestTelegramRetentionDeleteOfARemovedMessageSucceeds(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 1)
		h.deliver("payment.created", "created")
		h.tg.override = func(method string) (int, string, bool) {
			return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message to delete not found"}`, true
		}
		h.e.clk.Add(time.Hour)
		h.run(1)
		var failed int
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE kind = ? AND last_error IS NOT NULL AND last_error <> ''`,
				outbound.JobDeleteTelegram).Scan(&failed)
		}); err != nil {
			t.Fatal(err)
		}
		if failed != 0 {
			t.Fatalf("delete jobs recorded as failed: %d", failed)
		}
	})
}

func TestTelegramSeparateAlertSoundFollowsTheEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		sound  []string
		silent bool
	}{"ticked": {[]string{"refund.succeeded"}, false}, "unticked": {nil, true}} {
		t.Run(name, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				h := newTelegramHarness(t, db, 0)
				h.setSound(0, tc.sound...)
				h.write(func(tx store.Tx) error {
					return h.svc.Emit(ctx, tx, events.Event{OrgID: "org_1", AppID: "app_1", Mode: "test", Type: "refund.succeeded",
						ObjectType: "refund", ObjectID: "re_1", ObjectVersion: 1, Data: map[string]any{"id": "re_1"}})
				})
				h.run(1)
				wantCalls(t, h.tg.take(), telegramCall{Method: "sendMessage", Silent: tc.silent, Text: "[TEST] refund.succeeded · re_1"})
			})
		})
	}
}

func TestTelegramSoundKeepsOnlySelectedEventsAndDefaultsAreStored(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		fresh, err := h.svc.NotificationConfig(ctx, "org_1", "live")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"payment.succeeded", "refund.succeeded", "dispute.opened", "dispute.lost"}
		if !slices.Equal(fresh.TelegramSoundEvents, want) {
			t.Fatalf("defaults = %v", fresh.TelegramSoundEvents)
		}
		h.setSound(0, "payment.failed", "payment.failed", "dispute.lost")
		saved, err := h.svc.NotificationConfig(ctx, "org_1", "test")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(saved.TelegramSoundEvents, []string{"payment.failed"}) {
			t.Fatalf("saved sound = %v", saved.TelegramSoundEvents)
		}
	})
}

func TestTelegramTestMessage(t *testing.T) {
	const head = "99.00 USD · TEST · pay_test_message\n🇺🇸 US · buyer@example.com"
	const history = "\n3× Insufficient funds · last 07:59\n1× 3-D Secure failed · 07:58\n1× Do not honor · 07:57\n2× Card expired · last 07:56"
	for _, tc := range []struct {
		scenario, want string
		sound          bool
	}{
		{"created", "⏳ " + head, true},
		{"failed_once", "❌ " + head + "\n1× Insufficient funds · 07:59", false},
		{"failed_many", "❌ " + head + "\n7 failed attempts" + history, true},
		{"paid", "✅ " + head + "\nPaid 08:00 UTC", true},
		{"paid_after_failures", "✅ " + head + "\nPaid 08:00 UTC on attempt 8" + history, false},
		{"canceled", "🚫 " + head + "\nCanceled", false},
		{"expired", "⌛ " + head + "\nExpired", false},
		{"alert", "[TEST] refund.succeeded · re_test_message · 99.00 USD", true},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				h := newTelegramHarness(t, db, 0)
				if err := h.svc.SendTelegramTest(ctx, "org_1", "test", tc.scenario, tc.sound); err != nil {
					t.Fatal(err)
				}
				wantCalls(t, h.tg.take(), telegramCall{Method: "sendMessage", Silent: !tc.sound, Text: "🧪 Test message\n" + tc.want})
			})
		})
	}
}

func TestTelegramTestMessageUsesRetention(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 1)
		if err := h.svc.SendTelegramTest(ctx, "org_1", "test", "paid", true); err != nil {
			t.Fatal(err)
		}
		h.tg.take()
		h.e.clk.Add(time.Hour)
		h.run(1)
		wantCalls(t, h.tg.take(), telegramCall{Method: "deleteMessage", MessageID: 321})
	})
}

func TestTelegramTestMessageErrors(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		if err := h.svc.SendTelegramTest(ctx, "org_1", "test", "nonsense", true); err == nil {
			t.Fatal("unknown scenario accepted")
		}
		if err := h.svc.SendTelegramTest(ctx, "org_1", "live", "paid", true); err == nil || !strings.Contains(err.Error(), "bot token") {
			t.Fatalf("unconfigured mode: %v", err)
		}
		if calls := h.tg.take(); len(calls) != 0 {
			t.Fatalf("calls: %+v", calls)
		}
		h.tg.override = func(string) (int, string, bool) {
			return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: chat not found"}`, true
		}
		err := h.svc.SendTelegramTest(ctx, "org_1", "test", "paid", true)
		if err == nil || !strings.Contains(err.Error(), "chat not found") || strings.Contains(err.Error(), "ABC_def-789") {
			t.Fatalf("rejected: %v", err)
		}
	})
}

// Telegram message IDs are per chat, so two chats can share one.
func TestTelegramTestMessagesInTwoChatsWithOneMessageIDBothGetDeleted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 1)
		cfg := outbound.NotificationConfig{TelegramChatID: "-100999", TelegramEnabled: true, TelegramDeleteAfterHours: 1, EventTypes: lifecycleEvents}
		if err := h.svc.SaveNotificationConfig(ctx, "org_1", "live", cfg, "123456:ABC_def-789"); err != nil {
			t.Fatal(err)
		}
		h.tg.override = func(method string) (int, string, bool) {
			if method == "sendMessage" {
				return http.StatusOK, `{"ok":true,"result":{"message_id":500}}`, true
			}
			return 0, "", false
		}
		for _, mode := range []string{"test", "live"} {
			if err := h.svc.SendTelegramTest(ctx, "org_1", mode, "paid", false); err != nil {
				t.Fatal(err)
			}
		}
		h.tg.take()
		h.e.clk.Add(time.Hour)
		h.run(2)
		wantDeletes := 0
		for _, c := range h.tg.take() {
			if c.Method == "deleteMessage" && c.MessageID == 500 {
				wantDeletes++
			}
		}
		if wantDeletes != 2 {
			t.Fatalf("deleted %d test messages, want 2", wantDeletes)
		}
	})
}
