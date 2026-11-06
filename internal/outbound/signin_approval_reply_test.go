package outbound_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// sent seeds a user and a request whose notification is out, and returns its
// message id.
func (h *signinHarness) sent(userID, hash string) int64 {
	h.t.Helper()
	h.user(userID)
	h.begin(userID, hash)
	h.tick()
	return h.request(hash).MessageID
}

func TestCorrectReplyApprovesAndDeletesBothMessages(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		reply := h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved || a.DecidedAt.IsZero() || !a.Cleared {
			t.Fatalf("request %+v", a)
		}
		got := h.tg.Deleted()
		if len(got) != 2 || !slices.Contains(got, reply) || !slices.Contains(got, msg) {
			t.Fatalf("deleted %v, want %d and %d", got, reply, msg)
		}
		if n := h.tick(); n != 0 {
			t.Fatalf("still ticking: %d", n)
		}
	})
}

func TestReplyWithSpacesCounts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.Reply(msg, "  "+signinPIN+"\n")
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved {
			t.Fatalf("request %+v", a)
		}
	})
}

func TestNonTextReplyIsDeletedAndNotCounted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		reply := h.tg.Reply(msg, "")
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || a.WrongAttempts != 0 {
			t.Fatalf("request %+v", a)
		}
		if !slices.Contains(h.tg.Deleted(), reply) {
			t.Fatalf("deleted %v", h.tg.Deleted())
		}
	})
}

func TestWrongRepliesAreDeletedAndFiveEndTheRequest(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		var replies []int64
		for i := range 4 {
			replies = append(replies, h.tg.Reply(msg, fmt.Sprintf("11111%d", i)))
		}
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || a.WrongAttempts != 4 {
			t.Fatalf("after four: %+v", a)
		}
		replies = append(replies, h.tg.Reply(msg, "111119"))
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninEnded || a.WrongAttempts != 5 || !a.Cleared {
			t.Fatalf("after five: %+v", a)
		}
		deleted := h.tg.Deleted()
		for _, r := range append(replies, msg) {
			if !slices.Contains(deleted, r) {
				t.Fatalf("%d not deleted: %v", r, deleted)
			}
		}
		// A sixth reply cannot revive it: the right PIN changes nothing now.
		h.begin("usr_1", "b2")
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("revived: %+v", a)
		}
	})
}

func TestRepliesElsewhereAreIgnored(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.ReplyFromChat(-555, msg, signinPIN)
		h.tg.Reply(99999, signinPIN)
		h.tg.ReplyFromChat(-100123, 0, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending || a.WrongAttempts != 0 {
			t.Fatalf("request %+v", a)
		}
		if got := h.tg.Deleted(); len(got) != 0 {
			t.Fatalf("deleted someone else's messages: %v", got)
		}
		if h.tg.LastOffset() != 0 {
			t.Fatalf("first read from offset %d", h.tg.LastOffset())
		}
		h.tick()
		if h.tg.LastOffset() != 504 {
			t.Fatalf("offset %d, want past the ignored messages", h.tg.LastOffset())
		}
	})
}

func TestReplyToAnEndedRequestIsStillDeleted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		old := h.sent("usr_1", "b1")
		h.begin("usr_1", "b2")
		h.tick()
		reply := h.tg.Reply(old, signinPIN)
		h.tick()
		if !slices.Contains(h.tg.Deleted(), reply) {
			t.Fatalf("deleted %v", h.tg.Deleted())
		}
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("ended request changed: %+v", a)
		}
		if a := h.request("b2"); a.Status != store.SigninPending || a.WrongAttempts != 0 {
			t.Fatalf("live request changed: %+v", a)
		}
	})
}

func TestUsernameChatIDStillMatches(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.channel("@groupname", true, "")
		msg := h.sent("usr_1", "b1")
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved || a.ChatID != -100123 {
			t.Fatalf("request %+v", a)
		}
	})
}

func TestOffsetIsKeptPerBotAndAdvancesAfterHandling(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.Reply(msg, "111111")
		h.tick()
		h.tick()
		if h.tg.LastOffset() != 502 {
			t.Fatalf("offset %d, want one past update 501", h.tg.LastOffset())
		}
		// A new bot has its own update numbers, so it starts from the beginning.
		h.channel("-100123", true, "654321:NEW_token-123")
		h.tick()
		if h.tg.LastOffset() != 0 {
			t.Fatalf("new bot read from offset %d", h.tg.LastOffset())
		}
		h.channel("-100123", true, signinBotToken)
		h.tick()
		if h.tg.LastOffset() != 502 {
			t.Fatalf("old bot read from offset %d", h.tg.LastOffset())
		}
	})
}

func TestOverlappingPollsDoNotDoubleCount(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		// Another hub instance is reading.
		now := h.clk.Now()
		if _, ok, err := db.ClaimSigninPoll(ctx, "123456", now.Add(2*time.Second), now.Add(time.Minute)); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		before := h.tg.Calls("getUpdates")
		h.tg.Reply(msg, "111111")
		if n := h.tick(); n != 1 {
			t.Fatalf("ran %d", n)
		}
		if h.tg.Calls("getUpdates") != before {
			t.Fatal("read replies while another instance holds the claim")
		}
		if n := h.tick(); n != 1 {
			t.Fatalf("no next tick: %d", n)
		}
		if err := db.ReleaseSigninPoll(ctx, "123456", 0); err != nil {
			t.Fatal(err)
		}
		h.tick()
		if a := h.request("b1"); a.WrongAttempts != 1 {
			t.Fatalf("request %+v", a)
		}
	})
}

func TestWebhookSetLogsAWarningAndRequestExpires(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.SetWebhook("https://example.com/hook")
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninPending {
			t.Fatalf("request %+v", a)
		}
		if logs := h.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "409") {
			t.Fatalf("logs: %s", logs)
		}
		h.runOut()
		if a := h.request("b1"); a.Status != store.SigninEnded {
			t.Fatalf("after expiry: %+v", a)
		}
	})
}

func TestFailedDeleteStillApproves(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		h.tg.Fail("deleteMessage", http.StatusBadRequest)
		h.tg.Reply(msg, signinPIN)
		h.tick()
		if a := h.request("b1"); a.Status != store.SigninApproved {
			t.Fatalf("request %+v", a)
		}
		logs := h.logs.String()
		if !strings.Contains(logs, "level=WARN") || strings.Contains(logs, signinPIN) {
			t.Fatalf("logs: %s", logs)
		}
	})
}

func TestReplyTextIsStoredNowhere(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		const wrong = "777123"
		h.tg.Reply(msg, wrong)
		h.tg.Fail("deleteMessage", http.StatusBadRequest)
		h.tg.Reply(msg, signinPIN)
		h.tick()
		h.tg.Fail("deleteMessage", 0)
		h.tick()
		if logs := h.logs.String(); strings.Contains(logs, wrong) || strings.Contains(logs, signinPIN) {
			t.Fatalf("logs: %s", logs)
		}
		for _, table := range []string{"jobs", "signin_approvals", "signin_polls", "audit_log"} {
			rows, err := selectAll(db, table)
			if err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				for i, v := range vals {
					text := fmt.Sprint(v)
					if b, ok := v.([]byte); ok {
						text = string(b)
					}
					if strings.Contains(text, wrong) || strings.Contains(text, signinPIN) {
						t.Fatalf("%s.%s holds reply text: %s", table, cols[i], text)
					}
				}
			}
			rows.Close()
		}
	})
}
