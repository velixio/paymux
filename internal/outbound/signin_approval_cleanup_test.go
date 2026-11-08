package outbound_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// Telegram can take a notification and lose its answer. Then the request gets a
// second one, and a reply to the first must still be removed, with the first.
func TestAReplyToAnUntrackedNotificationIsDeleted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		h.tg.LoseAnswer("sendMessage", true)
		h.tick()
		h.tg.LoseAnswer("sendMessage", false)
		if a := h.request("b1"); a.MessageID != 0 || len(h.tg.Sent()) != 1 {
			t.Fatalf("after the lost answer: %+v, sent %+v", a, h.tg.Sent())
		}
		lost := h.tg.Sent()[0].MessageID
		h.tick()
		if a := h.request("b1"); a.MessageID == 0 || a.MessageID == lost {
			t.Fatalf("no second notification: %+v", a)
		}
		reply := h.tg.Reply(lost, signinPIN)
		other := h.tg.ReplyFromChat(-100123, 5555, signinPIN)
		h.tick()
		deleted := h.tg.Deleted()
		if !slices.Contains(deleted, reply) || !slices.Contains(deleted, lost) {
			t.Fatalf("reply %d or notification %d left in the chat, deleted %v", reply, lost, deleted)
		}
		if slices.Contains(deleted, other) {
			t.Fatalf("deleted a reply to a message the bot did not send: %v", deleted)
		}
		if a := h.request("b1"); a.Status != store.SigninPending || a.WrongAttempts != 0 {
			t.Fatalf("an untracked reply decided the request: %+v", a)
		}
	})
}

// A reply Telegram would not delete is tried again until it is gone.
func TestAReplyThatCouldNotBeDeletedIsRetried(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.Fail("deleteMessage", http.StatusInternalServerError)
		reply := h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved {
			t.Fatalf("the PIN was not taken: %+v", a)
		}
		h.tg.Fail("deleteMessage", 0)
		h.clk.Add(5 * time.Minute)
		h.tick()
		if !slices.Contains(h.tg.Deleted(), reply) {
			t.Fatalf("reply %d left in the chat, deleted %v", reply, h.tg.Deleted())
		}
		h.clk.Add(5 * time.Minute)
		if n := h.tick(); n != 0 {
			t.Fatalf("still ticking when all is gone: %d", n)
		}
	})
}

// Past Telegram's two-day limit a message can no longer be deleted, so trying stops.
func TestRetryingADeletionStopsAtTelegramsLimit(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.Fail("deleteMessage", http.StatusBadRequest)
		h.tg.Reply(msg, signinPIN)
		h.tick()
		h.clk.Add(48 * time.Hour)
		h.tick()
		h.clk.Add(5 * time.Minute)
		if n := h.tick(); n != 0 {
			t.Fatalf("still trying after two days: %d", n)
		}
	})
}
