package outbound_test

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func selectAll(db *store.DB, table string) (*sql.Rows, error) {
	var rows *sql.Rows
	err := db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = q.QueryContext(ctx, `SELECT * FROM `+table)
		return err
	})
	return rows, err
}

// Requests started together queue ticks that run at the same time.
func TestTicksRunningTogetherSendEachNotificationOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.user("usr_2")
		h.user("usr_3")
		h.begin("usr_1", "b1")
		h.begin("usr_2", "b2")
		h.begin("usr_3", "b3")
		if _, err := h.q.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		h.tick()
		if n := len(h.tg.Sent()); n != 3 {
			t.Fatalf("sent %d messages for 3 requests: %+v", n, h.tg.Sent())
		}
	})
}

// While another worker holds the tick lease this one sends and reads nothing, and
// only keeps the chain going.
func TestATickDoesNoWorkWhileTheLeaseIsHeld(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		now := h.clk.Now()
		until := now.Add(time.Minute)
		if _, ok, err := db.ClaimSigninPoll(ctx, "tick", now, until); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if n := h.tick(); n != 1 {
			t.Fatalf("ran %d", n)
		}
		if n := len(h.tg.Sent()); n != 0 || h.tg.Calls("getUpdates") != 0 {
			t.Fatalf("worked without the lease: sent %d, read %d", n, h.tg.Calls("getUpdates"))
		}
		if err := db.ReleaseSigninPoll(ctx, "tick", until, 0); err != nil {
			t.Fatal(err)
		}
		if n := h.tick(); n != 1 || len(h.tg.Sent()) != 1 {
			t.Fatalf("after release: ran %d, sent %d", n, len(h.tg.Sent()))
		}
	})
}

// A tick stops before its lease runs out however slow Telegram is, so no other
// tick can be working at the same time, and running out of time is not a failure.
func TestATickStopsBeforeItsLeaseRunsOut(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		limit := h.controlTimeLimit()
		h.user("usr_1")
		h.begin("usr_1", "b1")
		var ran atomic.Bool
		h.tg.Before("sendMessage", func(r *http.Request) {
			ran.Store(true)
			limit.runOut()
			waitStopped(t, r)
		})
		if n := h.tick(); n != 1 {
			t.Fatalf("ran %d", n)
		}
		if !ran.Load() {
			t.Fatal("the notification was never tried")
		}
		h.tg.Before("sendMessage", nil)
		if n := h.tick(); n != 1 {
			t.Fatalf("the next tick is missing or the lease is stuck: ran %d", n)
		}
		if a := h.request("b1"); a.MessageID == 0 {
			t.Fatalf("not sent by the next tick: %+v", a)
		}
	})
}

// If another worker recorded a message first, this worker's message is removed, so
// no notification is left in the chat that replies cannot be matched to.
func TestASecondNotificationForARequestIsDeleted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		h.user("usr_1")
		h.begin("usr_1", "b1")
		id := h.request("b1").ID
		h.tg.Before("sendMessage", func(*http.Request) {
			if _, err := db.SetSigninNotification(ctx, id, -100123, 999); err != nil {
				t.Error(err)
			}
		})
		h.tick()
		sent := h.tg.Sent()
		if len(sent) != 1 {
			t.Fatalf("sent %+v", sent)
		}
		if a := h.request("b1"); a.MessageID != 999 {
			t.Fatalf("the first message was replaced: %+v", a)
		}
		if !slices.Contains(h.tg.Deleted(), sent[0].MessageID) {
			t.Fatalf("message %d left in the chat, deleted %v", sent[0].MessageID, h.tg.Deleted())
		}
	})
}

// A reply whose deletion was cut off by the time limit is read again, not skipped,
// because it may hold a PIN.
func TestAReplyCutOffByTheTimeLimitIsDeletedLater(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newSigninHarness(t, db)
		msg := h.sent("usr_1", "b1")
		reply := h.tg.Reply(msg, "")
		limit := h.controlTimeLimit()
		var ran atomic.Bool
		h.tg.Before("deleteMessage", func(r *http.Request) {
			ran.Store(true)
			limit.runOut()
			waitStopped(t, r)
		})
		h.tick()
		if !ran.Load() {
			t.Fatal("the reply was never deleted")
		}
		h.tg.Before("deleteMessage", nil)
		h.tick()
		if !slices.Contains(h.tg.Deleted(), reply) {
			t.Fatalf("reply %d left in the chat, deleted %v", reply, h.tg.Deleted())
		}
	})
}

// timeLimit is a tick's time limit that runs out when the test says so.
type timeLimit struct {
	mu  sync.Mutex
	cur *limitCtx
}

type limitCtx struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func (c *limitCtx) Done() <-chan struct{} { return c.done }

func (c *limitCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return c.Context.Err()
	}
}

// controlTimeLimit makes each tick's time limit run out only on runOut.
func (h *signinHarness) controlTimeLimit() *timeLimit {
	l := &timeLimit{}
	h.svc.SetSigninWorkContext(func(ctx context.Context) (context.Context, context.CancelFunc) {
		c := &limitCtx{Context: ctx, done: make(chan struct{})}
		l.mu.Lock()
		l.cur = c
		l.mu.Unlock()
		return c, func() {}
	})
	h.t.Cleanup(func() { h.svc.SetSigninWorkContext(nil) })
	return l
}

func (l *timeLimit) runOut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cur.once.Do(func() { close(l.cur.done) })
}

// waitStopped holds a Telegram call until the tick's time limit stops it.
func waitStopped(t *testing.T, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
		t.Error("the call was not stopped by the time limit")
	}
}
