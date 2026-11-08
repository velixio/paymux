package outbound_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const signinPIN = "482913"

// signinTelegramUser is the Telegram account the seeded users link: the fake's
// private chat.
const signinTelegramUser = 42

// user seeds a user linked to Telegram who approves with signinPIN.
func (h *signinHarness) user(id string) {
	h.t.Helper()
	enc, err := h.box.Seal([]byte(signinPIN), secrets.AADSigninPIN(id))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.db.InsertUser(ctx, store.UserRow{ID: id, Email: id + "@example.com", PasswordHash: "h", Role: "admin", SigninPINEnc: enc,
		TelegramUserID: signinTelegramUser, SigninMethod: store.SigninMethodPIN, CreatedAt: h.clk.Now()}); err != nil {
		h.t.Fatal(err)
	}
}

// tapUser seeds a user linked to Telegram who approves with a tap.
func (h *signinHarness) tapUser(id string) {
	h.t.Helper()
	if err := h.db.InsertUser(ctx, store.UserRow{ID: id, Email: id + "@example.com", PasswordHash: "h", Role: "admin",
		TelegramUserID: signinTelegramUser, SigninMethod: store.SigninMethodTap, CreatedAt: h.clk.Now()}); err != nil {
		h.t.Fatal(err)
	}
}

// begin starts a request for a user, with the user's method; hash is what finds
// it again.
func (h *signinHarness) begin(userID, hash string) {
	h.t.Helper()
	u, err := h.db.UserByID(ctx, userID)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.svc.BeginSigninApproval(ctx, outbound.SigninApprovalInput{OrgID: "org_1", UserID: userID, BrowserHash: hash, IP: "203.0.113.9",
		Method: u.SigninMethod, Remember: true}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *signinHarness) request(hash string) store.SigninApproval {
	h.t.Helper()
	a, err := h.db.SigninApprovalByBrowser(ctx, hash)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// tick moves the clock to the next tick and runs what is due.
func (h *signinHarness) tick() int {
	h.t.Helper()
	h.clk.Add(2 * time.Second)
	n, err := h.q.RunOnce(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// runOut ticks past the request's five minutes and until nothing is left to do.
func (h *signinHarness) runOut() {
	h.t.Helper()
	for range 160 {
		h.tick()
	}
}

func TestSigninNotificationIsSentWithSoundAndAsksForAReply(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		// No events, no sound, one-hour auto-delete: none of them apply here.
		cfg := outbound.NotificationConfig{TelegramChatID: "-100123", TelegramEnabled: true, TelegramDeleteAfterHours: 1}
		if err := h.svc.SaveNotificationConfig(ctx, "org_1", "live", cfg, ""); err != nil {
			t.Fatal(err)
		}
		h.user("usr_1")
		h.begin("usr_1", "b1")
		if n, err := h.q.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("ran %d: %v", n, err)
		}
		sent := h.tg.Sent()
		if len(sent) != 1 {
			t.Fatalf("sent %+v", sent)
		}
		want := "Sign-in request\nusr_1@example.com\nIP: 203.0.113.9\nTime: 2027-01-15T08:00:00Z\nReply to this message with your PIN."
		if m := sent[0]; m.ChatID != "42" || m.DisableNotification || !m.ForceReply || len(m.Buttons) != 0 || m.Text != want {
			t.Fatalf("message %+v", m)
		}
		a := h.request("b1")
		if a.ChatID != signinTelegramUser || a.MessageID != sent[0].MessageID || a.Status != store.SigninPending || !a.ExpiresAt.Equal(a.CreatedAt.Add(5*time.Minute)) {
			t.Fatalf("request %+v", a)
		}
		var deletes int
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE kind = ?`, outbound.JobDeleteTelegram).Scan(&deletes)
		}); err != nil || deletes != 0 {
			t.Fatalf("auto-delete jobs %d: %v", deletes, err)
		}
		h.tick()
		if n := len(h.tg.Sent()); n != 1 {
			t.Fatalf("sent %d times", n)
		}
	})
}

func TestSendingRetriesUntilItWorks(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.tg.Fail("sendMessage", http.StatusInternalServerError)
		h.begin("usr_1", "b1")
		h.tick()
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || a.MessageID != 0 {
			t.Fatalf("while failing: %+v", a)
		}
		h.tg.Fail("sendMessage", 0)
		h.tick()
		if len(h.tg.Sent()) != 1 || h.request("b1").MessageID == 0 {
			t.Fatalf("after the fault: %+v", h.tg.Sent())
		}
	})
}

func TestSendingNeverWorkingEndsAtExpiryAndStopsTicking(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.tg.Fail("sendMessage", http.StatusInternalServerError)
		h.begin("usr_1", "b1")
		h.runOut()
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("after expiry: %+v", a)
		}
		if n := h.tick(); n != 0 {
			t.Fatalf("still ticking: %d", n)
		}
	})
}

func TestExpiryDeletesTheNotificationOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		h.tick()
		msg := h.request("b1").MessageID
		h.runOut()
		if got := h.tg.Deleted(); len(got) != 1 || got[0] != msg {
			t.Fatalf("deleted %v, want %d once", got, msg)
		}
		if !h.request("b1").Cleared {
			t.Fatal("not marked cleared")
		}
	})
}

func TestASecondSignInEndsTheFirstAndDeletesItsNotification(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		h.tick()
		first := h.request("b1").MessageID
		h.begin("usr_1", "b2")
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninEnded || !a.Cleared {
			t.Fatalf("first: %+v", a)
		}
		if got := h.tg.Deleted(); len(got) != 1 || got[0] != first {
			t.Fatalf("deleted %v", got)
		}
		if a := h.request("b2"); a.Status != store.SigninPending || a.MessageID == 0 || a.MessageID == first {
			t.Fatalf("second: %+v", a)
		}
	})
}

func TestAFailedDeleteOfTheNotificationIsRetried(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		h.tick()
		h.tg.Fail("deleteMessage", http.StatusInternalServerError)
		h.begin("usr_1", "b2")
		h.tick()
		if h.request("b1").Cleared {
			t.Fatal("marked cleared while the delete fails")
		}
		h.tg.Fail("deleteMessage", 0)
		h.tick()
		if !h.request("b1").Cleared || len(h.tg.Deleted()) != 1 {
			t.Fatalf("after the fault: %+v %v", h.request("b1"), h.tg.Deleted())
		}
	})
}

func TestAFailedSendLeavesNoSecretInTheJobOrLogs(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.tg.Fail("sendMessage", http.StatusUnauthorized)
		h.begin("usr_1", "b1")
		h.tick()
		var payload, lastError string
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT payload, COALESCE(last_error, '') FROM jobs WHERE kind = ? ORDER BY created_at LIMIT 1`, outbound.JobSigninApprovalTick).Scan(&payload, &lastError)
		}); err != nil {
			t.Fatal(err)
		}
		if payload != "{}" || lastError != "" {
			t.Fatalf("job payload %q, last error %q", payload, lastError)
		}
		logs := h.logs.String()
		if !strings.Contains(logs, "401") || strings.Contains(logs, signinBotToken) || strings.Contains(logs, signinPIN) {
			t.Fatalf("logs: %s", logs)
		}
	})
}

func TestTickStopsWhenNothingIsOpen(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		if err := db.ClearUserSigninApproval(ctx, "usr_1", h.clk.Now()); err != nil {
			t.Fatal(err)
		}
		if n := h.tick(); n != 1 {
			t.Fatalf("ran %d", n)
		}
		if n := h.tick(); n != 0 {
			t.Fatalf("a tick was queued with nothing open: %d", n)
		}
		if h.tg.Calls("getUpdates") != 0 || h.tg.Calls("sendMessage") != 0 {
			t.Fatal("called Telegram with nothing to do")
		}
	})
}

func TestChannelOffFailsClosed(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.channel("-100123", false, "")
		h.begin("usr_1", "b1")
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || len(h.tg.Sent()) != 0 {
			t.Fatalf("with the channel off: %+v, sent %v", a, h.tg.Sent())
		}
		if !strings.Contains(h.logs.String(), "level=WARN") {
			t.Fatalf("no warning: %s", h.logs.String())
		}
		h.runOut()
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("after expiry: %+v", a)
		}
	})
}

func TestOldEndedRequestsArePurged(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "old")
		h.tick()
		h.clk.Add(49 * time.Hour)
		h.begin("usr_1", "new")
		h.tick()
		if _, err := db.SigninApprovalByBrowser(ctx, "old"); err != store.ErrNotFound {
			t.Fatalf("old request: %v", err)
		}
		h.request("new")
	})
}
