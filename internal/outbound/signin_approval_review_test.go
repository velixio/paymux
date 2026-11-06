package outbound_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// A busy group fills getUpdates with ordinary messages. They must not stop the
// reply behind them from being read.
func TestALargeBacklogDoesNotStopReplies(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		long := strings.Repeat("x", 700)
		for i := range 250 {
			h.tg.ReplyFromChat(-100123, 0, fmt.Sprintf("%d %s", i, long))
		}
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved {
			t.Fatalf("after a 250-message backlog: %+v\nlogs: %s", a, h.logs.String())
		}
	})
}

// One tick reads a bounded amount, and the next tick carries on from there.
func TestAVeryLargeBacklogIsReadOverSeveralTicks(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		for i := range 2500 {
			h.tg.ReplyFromChat(-100123, 0, fmt.Sprintf("%d", i))
		}
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending {
			t.Fatalf("first tick read everything: %+v", a)
		}
		h.tick()
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved {
			t.Fatalf("after three ticks: %+v", a)
		}
	})
}

// A notification that cannot be deleted must not keep the two-second tick going.
func TestAStuckNotificationIsRetriedSlowly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.sent("usr_1", "b1")
		h.tg.Fail("deleteMessage", http.StatusInternalServerError)
		h.clk.Add(5 * time.Minute)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninEnded || a.Cleared {
			t.Fatalf("after expiry: %+v", a)
		}
		tried := h.tg.Calls("deleteMessage")
		for range 30 {
			if n := h.tick(); n != 0 {
				t.Fatalf("a tick ran within the minute after: %d", n)
			}
		}
		if got := h.tg.Calls("deleteMessage"); got != tried {
			t.Fatalf("%d delete calls in a minute, want none", got-tried)
		}
		h.clk.Add(5 * time.Minute)
		if n := h.tick(); n != 1 {
			t.Fatalf("no slow retry: %d", n)
		}
		if got := h.tg.Calls("deleteMessage"); got != tried+1 {
			t.Fatalf("%d delete calls on the slow retry, want 1", got-tried)
		}
		h.tg.Fail("deleteMessage", 0)
		h.clk.Add(5 * time.Minute)
		h.tick()
		if !h.request("b1").Cleared {
			t.Fatal("not cleared once deleting works")
		}
		h.clk.Add(5 * time.Minute)
		if n := h.tick(); n != 0 {
			t.Fatalf("still ticking when all is done: %d", n)
		}
	})
}

// A new request is served at the fast pace even while an old notification is stuck.
func TestANewRequestIsNotHeldBackByAStuckNotification(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.sent("usr_1", "b1")
		h.tg.Fail("deleteMessage", http.StatusInternalServerError)
		h.clk.Add(5 * time.Minute)
		h.tick()
		h.begin("usr_1", "b2")
		h.tick()
		msg := h.request("b2").MessageID
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b2"); a.Status != store.SigninApproved {
			t.Fatalf("second request: %+v", a)
		}
	})
}
