package outbound_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/outbound/telegramtest"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const (
	tapApprove = "signin:approve"
	tapDeny    = "signin:deny"
)

// tapSent seeds a tap user and a request whose notification is out, and returns
// its message id.
func (h *signinHarness) tapSent(userID, hash string) int64 {
	h.t.Helper()
	h.tapUser(userID)
	h.begin(userID, hash)
	h.tick()
	return h.request(hash).MessageID
}

func (h *signinHarness) lastAnswer() string {
	h.t.Helper()
	a := h.tg.Answers()
	if len(a) == 0 {
		h.t.Fatal("no tap was answered")
	}
	return a[len(a)-1].Text
}

func TestTapNotificationGoesToThePrivateChatWithButtons(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.tapSent("usr_1", "b1")
		sent := h.tg.Sent()
		want := "Sign-in request\nusr_1@example.com\nIP: 203.0.113.9\nTime: 2027-01-15T08:00:00Z\nApprove only if this is you."
		if len(sent) != 1 || sent[0].ChatID != "42" || sent[0].ForceReply || sent[0].Text != want ||
			!slices.Equal(sent[0].Buttons, []string{tapApprove, tapDeny}) {
			t.Fatalf("sent %+v", sent)
		}
	})
}

func TestTappingApproveApprovesAndRemovesTheNotification(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.tapSent("usr_1", "b1")
		h.tg.Tap(msg, tapApprove)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved || !a.Cleared {
			t.Fatalf("request %+v", a)
		}
		if got := h.lastAnswer(); got != "Approved. Your browser will sign in." {
			t.Fatalf("answer %q", got)
		}
		if !slices.Contains(h.tg.Deleted(), msg) {
			t.Fatalf("notification left: deleted %v", h.tg.Deleted())
		}
	})
}

func TestTappingDenyEndsTheRequest(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.tapSent("usr_1", "b1")
		h.tg.Tap(msg, tapDeny)
		h.tg.Tap(msg, tapApprove)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("request %+v", a)
		}
		answers := h.tg.Answers()
		if len(answers) != 2 || answers[0].Text != "Denied. That sign-in will not go ahead." || answers[1].Text != "This sign-in request has ended." {
			t.Fatalf("answers %+v", answers)
		}
	})
}

func TestTapsThatAreNotTheUsersDecideNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.tapSent("usr_1", "b1")
		h.tg.TapFrom(signinTelegramUser, 77, msg, tapApprove) // someone else
		h.tg.Tap(msg, "something else")
		h.tg.Tap(msg+1, tapApprove) // not the notification
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending {
			t.Fatalf("request %+v", a)
		}
		if n := len(h.tg.Answers()); n != 3 {
			t.Fatalf("%d of 3 taps answered", n)
		}
	})
}

func TestATapOnAPINRequestDecidesNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.Tap(msg, tapApprove)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending {
			t.Fatalf("request %+v", a)
		}
	})
}

func TestAReplyToATapRequestIsDeletedAndDecidesNothing(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.tapSent("usr_1", "b1")
		reply := h.tg.Reply(msg, "yes")
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || a.WrongAttempts != 0 {
			t.Fatalf("request %+v", a)
		}
		if !slices.Contains(h.tg.Deleted(), reply) {
			t.Fatalf("reply left: deleted %v", h.tg.Deleted())
		}
	})
}

func TestAPINFromAnotherSenderIsDeletedAndNotCounted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		reply := h.tg.ReplyFrom(signinTelegramUser, 77, msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || a.WrongAttempts != 0 {
			t.Fatalf("request %+v", a)
		}
		if !slices.Contains(h.tg.Deleted(), reply) {
			t.Fatalf("reply left: deleted %v", h.tg.Deleted())
		}
	})
}

func TestATapOnAnUntrackedNotificationRemovesIt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.tapUser("usr_1")
		h.begin("usr_1", "b1")
		h.tg.LoseAnswer("sendMessage", true)
		h.tick()
		h.tg.LoseAnswer("sendMessage", false)
		lost := h.tg.Sent()[0].MessageID
		h.tick()
		h.tg.Tap(lost, tapApprove)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending {
			t.Fatalf("an untracked notification decided the request: %+v", a)
		}
		if !slices.Contains(h.tg.Deleted(), lost) {
			t.Fatalf("untracked notification left: deleted %v", h.tg.Deleted())
		}
	})
}

// link makes a link for the user and returns its code.
func (h *signinHarness) link(userID string) string {
	h.t.Helper()
	url, err := h.svc.BeginTelegramLink(ctx, "org_1", userID)
	if err != nil {
		h.t.Fatal(err)
	}
	code, ok := strings.CutPrefix(url, "https://t.me/"+telegramtest.BotUsername+"?start=")
	if !ok || len(code) < 40 {
		h.t.Fatalf("link %q", url)
	}
	return code
}

func (h *signinHarness) plainUser(id string) {
	h.t.Helper()
	if err := h.db.InsertUser(ctx, store.UserRow{ID: id, Email: id + "@example.com", PasswordHash: "h", Role: "admin", CreatedAt: h.clk.Now()}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *signinHarness) telegramOf(id string) int64 {
	h.t.Helper()
	u, err := h.db.UserByID(ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return u.TelegramUserID
}

func (h *signinHarness) lastSentText() string {
	h.t.Helper()
	sent := h.tg.Sent()
	if len(sent) == 0 {
		h.t.Fatal("nothing sent")
	}
	return sent[len(sent)-1].Text
}

func TestOpeningTheLinkConnectsTelegramOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.plainUser("usr_1")
		code := h.link("usr_1")
		h.tg.Start(55, code)
		h.tick()
		if got := h.telegramOf("usr_1"); got != 55 {
			t.Fatalf("linked %d", got)
		}
		if m := h.tg.Sent(); len(m) != 1 || m[0].ChatID != "55" || m[0].Text != "Connected to Paymux as usr_1@example.com. You can now turn on sign-in approval in Paymux." {
			t.Fatalf("sent %+v", m)
		}
		// The bot reads again only while something waits, here another user's link.
		h.plainUser("usr_2")
		h.link("usr_2")
		h.tg.Start(66, code)
		h.tick()
		if got := h.telegramOf("usr_1"); got != 55 {
			t.Fatalf("a spent link linked %d", got)
		}
		if got := h.lastSentText(); !strings.HasPrefix(got, "This link has expired or was already used.") {
			t.Fatalf("answer %q", got)
		}
	})
}

func TestALinkStopsWorkingAfterTenMinutes(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.plainUser("usr_1")
		code := h.link("usr_1")
		h.clk.Add(10 * time.Minute)
		h.tg.Start(55, code)
		h.tick()
		if got := h.telegramOf("usr_1"); got != 0 {
			t.Fatalf("an expired link linked %d", got)
		}
		for range 5 {
			h.tick()
		}
		if n := h.tick(); n != 0 {
			t.Fatalf("still ticking after the link ended: %d", n)
		}
	})
}

func TestANewLinkReplacesTheOldOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.plainUser("usr_1")
		old := h.link("usr_1")
		h.link("usr_1")
		h.tg.Start(55, old)
		h.tick()
		if got := h.telegramOf("usr_1"); got != 0 {
			t.Fatalf("the old link linked %d", got)
		}
	})
}

func TestTheLinkWorksOnlyInAPrivateChat(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.plainUser("usr_1")
		code := h.link("usr_1")
		h.tg.ReplyFrom(-100123, 55, 0, "/start "+code)
		h.tick()
		if got := h.telegramOf("usr_1"); got != 0 {
			t.Fatalf("linked %d from a group", got)
		}
	})
}

func TestTheLinkNeedsAWorkingBot(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.plainUser("usr_1")
		h.tg.SetWebhook("https://example.com/hook")
		if _, err := h.svc.BeginTelegramLink(ctx, "org_1", "usr_1"); err == nil || err.Error() != "This bot has a webhook set. Remove it so Paymux can read replies." {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRelinkingEndsOpenRequests(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.tapSent("usr_1", "b1")
		code := h.link("usr_1")
		h.tg.Start(55, code)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("request %+v", a)
		}
	})
}
