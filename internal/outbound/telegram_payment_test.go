package outbound_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

type telegramCall struct {
	Method    string
	Text      string
	MessageID int64
	Silent    bool
}

// fakeTelegram answers the Bot API; override, when set, can reject a method.
type fakeTelegram struct {
	mu       sync.Mutex
	calls    []telegramCall
	nextID   int64
	override func(method string) (int, string, bool)
	failWith error
}

func (f *fakeTelegram) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Text      string `json:"text"`
		MessageID int64  `json:"message_id"`
		Silent    bool   `json:"disable_notification"`
	}
	_ = json.Unmarshal(raw, &body)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, telegramCall{method, body.Text, body.MessageID, body.Silent})
	if f.failWith != nil {
		return nil, f.failWith
	}
	status, response := http.StatusOK, `{"ok":true,"result":true}`
	if method == "sendMessage" {
		f.nextID++
		response = fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, f.nextID)
	}
	if f.override != nil {
		if s, b, ok := f.override(method); ok {
			status, response = s, b
		}
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
}

func (f *fakeTelegram) take() []telegramCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

type telegramHarness struct {
	t      *testing.T
	db     *store.DB
	e      *env
	q      *jobs.Queue
	svc    *outbound.Service
	tg     *fakeTelegram
	mailer *notificationMailer
	ver    int64
}

var (
	lifecycleEvents = []string{"payment.created", "payment.processing", "payment.failed", "payment.succeeded", "payment.canceled", "payment.needs_review"}
	testDay         = time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
)

func at(hour, minute int) time.Time {
	return testDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

func newTelegramHarness(t *testing.T, db *store.DB, retentionHours int) *telegramHarness {
	t.Helper()
	e := newEnv(t, db, true)
	h := &telegramHarness{t: t, db: db, e: e, tg: &fakeTelegram{nextID: 320}, mailer: &notificationMailer{}}
	h.q = jobs.New(db, jobs.Options{Clock: e.clk.Now, Logger: slog.New(slog.DiscardHandler)})
	h.svc = outbound.New(outbound.Options{DB: db, Queue: h.q, Box: e.box, Now: e.clk.Now, Mailer: h.mailer,
		NotificationClient: &http.Client{Transport: h.tg}})
	h.svc.Register()
	cfg := outbound.NotificationConfig{TelegramChatID: "-100123", TelegramEnabled: true, Email: "ops@example.com", EmailEnabled: true,
		TelegramDeleteAfterHours: retentionHours, EventTypes: lifecycleEvents}
	if err := h.svc.SaveNotificationConfig(ctx, "org_1", "test", cfg, "123456:ABC_def-789"); err != nil {
		t.Fatal(err)
	}
	h.write(func(tx store.Tx) error {
		if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_1", OrgID: "org_1", Mode: "test",
			Connector: "fake", Name: "fake", Status: "active", CreatedAt: h.e.clk.Now()}); err != nil {
			return err
		}
		customerID, err := store.UpsertCustomer(ctx, tx, store.Customer{ID: "cus_1", OrgID: "org_1", AppID: "app_1", Mode: "test",
			Email: "buyer@example.com", Billing: store.Billing{Country: "VN"}}, h.e.clk.Now())
		if err != nil {
			return err
		}
		return store.InsertPayment(ctx, tx, store.Payment{ID: "p123", OrgID: "org_1", AppID: "app_1", Mode: "test",
			Kind: "one_time", Amount: 9900, Currency: "USD", Status: "created", CustomerID: customerID,
			Metadata: map[string]string{}, ClientSecretHash: "hash", ExpiresAt: h.e.clk.Now().Add(time.Hour), Version: 1,
			CreatedAt: h.e.clk.Now(), UpdatedAt: h.e.clk.Now()})
	})
	return h
}

func (h *telegramHarness) write(fn func(store.Tx) error) {
	h.t.Helper()
	if err := h.db.Write(ctx, fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *telegramHarness) attempt(id, status, code, message string, updated time.Time) {
	h.t.Helper()
	h.write(func(tx store.Tx) error {
		return store.InsertAttempt(ctx, tx, store.Attempt{ID: id, OrgID: "org_1", Mode: "test", PaymentID: "p123",
			ProviderAccountID: "pa_1", ProviderRef: "ref_" + id, Status: status, FailureCode: code, FailureMessage: message,
			CreatedAt: updated, UpdatedAt: updated})
	})
}

// emit sets the payment's status and emits typ, without running the job.
func (h *telegramHarness) emit(typ, status string) {
	h.t.Helper()
	h.ver++
	h.write(func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE payments SET status = ? WHERE id = 'p123'`, status); err != nil {
			return err
		}
		return h.svc.Emit(ctx, tx, events.Event{OrgID: "org_1", AppID: "app_1", Mode: "test", Type: typ,
			ObjectType: "payment", ObjectID: "p123", ObjectVersion: h.ver, Data: map[string]any{"id": "p123"}})
	})
}

func (h *telegramHarness) run(want int) {
	h.t.Helper()
	ran, err := h.q.RunOnce(ctx)
	if err != nil || ran != want {
		h.t.Fatalf("jobs run = %d, want %d: %v", ran, want, err)
	}
}

// deliver emits typ and runs its notification job.
func (h *telegramHarness) deliver(typ, status string) []telegramCall {
	h.t.Helper()
	h.emit(typ, status)
	h.run(1)
	return h.tg.take()
}

func wantCalls(t *testing.T, got []telegramCall, want ...telegramCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("telegram calls:\n got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("telegram call %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

const telegramHead = "99.00 USD · TEST · p123\n🇻🇳 VN · buyer@example.com"

func TestTelegramPaymentLifecycleEditsOneMessage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 47)

		wantCalls(t, h.deliver("payment.created", "created"),
			telegramCall{Method: "sendMessage", Silent: true, Text: "⏳ " + telegramHead})

		h.attempt("a1", "failed", "card_declined", "Insufficient funds", at(16, 7))
		wantCalls(t, h.deliver("payment.failed", "failed"),
			telegramCall{Method: "editMessageText", MessageID: 321,
				Text: "❌ " + telegramHead + "\n1× Insufficient funds · 16:07"})

		h.attempt("a2", "captured", "", "", at(16, 9))
		wantCalls(t, h.deliver("payment.succeeded", "succeeded"),
			telegramCall{Method: "editMessageText", MessageID: 321,
				Text: "✅ " + telegramHead + "\nPaid 16:09 UTC on attempt 2\n1× Insufficient funds · 16:07"})

		if len(h.mailer.messages) != 3 {
			t.Fatalf("email stays per event, got %d messages", len(h.mailer.messages))
		}
		// Only the message's first send schedules its deletion.
		h.e.clk.Add(47 * time.Hour)
		h.run(1)
		wantCalls(t, h.tg.take(), telegramCall{Method: "deleteMessage", MessageID: 321})
	})
}

func TestTelegramPaymentMessageRendering(t *testing.T) {
	sevenFailures := func(h *telegramHarness) {
		h.attempt("a1", "failed", "", "Insufficient funds", at(16, 5))
		h.attempt("a2", "failed", "", "Insufficient funds", at(16, 6))
		h.attempt("a3", "failed", "", "Card expired", at(16, 7))
		h.attempt("a4", "failed", "", "Card expired", at(16, 8))
		h.attempt("a5", "failed", "", "Do not honor", at(16, 9))
		h.attempt("a6", "failed", "", "3-D Secure failed", at(16, 11))
		h.attempt("a7", "failed", "", "Insufficient funds", at(16, 12))
	}
	const sevenLines = "\n3× Insufficient funds · last 16:12\n1× 3-D Secure failed · 16:11\n1× Do not honor · 16:09\n2× Card expired · last 16:08"
	for name, tc := range map[string]struct {
		setup       func(*telegramHarness)
		typ, status string
		want        string
	}{
		"paid first try": {func(h *telegramHarness) { h.attempt("a1", "captured", "", "", at(16, 7)) },
			"payment.succeeded", "succeeded", "✅ " + telegramHead + "\nPaid 16:07 UTC"},
		"seven failures, latest reason first": {sevenFailures, "payment.failed", "failed",
			"❌ " + telegramHead + "\n7 failed attempts" + sevenLines},
		"paid after seven failures": {func(h *telegramHarness) {
			sevenFailures(h)
			h.attempt("a8", "captured", "", "", at(16, 14))
		}, "payment.succeeded", "succeeded", "✅ " + telegramHead + "\nPaid 16:14 UTC on attempt 8" + sevenLines},
		"reason falls back from message to code to a default": {func(h *telegramHarness) {
			h.attempt("a1", "failed", "do_not_honor", "", at(16, 7))
			h.attempt("a2", "failed", "", "", at(16, 8))
		}, "payment.failed", "failed",
			"❌ " + telegramHead + "\n2 failed attempts\n1× No provider reason · 16:08\n1× do_not_honor · 16:07"},
		"long and multi-line reasons are shortened": {func(h *telegramHarness) {
			h.attempt("a1", "failed", "", "Your card was\ndeclined "+strings.Repeat("x", 80), at(16, 7))
		}, "payment.failed", "failed",
			"❌ " + telegramHead + "\n1× Your card was declined " + strings.Repeat("x", 36) + "… · 16:07"},
		"canceled keeps the failure history": {func(h *telegramHarness) {
			h.attempt("a1", "failed", "", "Insufficient funds", at(16, 7))
		}, "payment.canceled", "canceled",
			"🚫 " + telegramHead + "\nCanceled\n1× Insufficient funds · 16:07"},
		"at most ten reasons are listed": {func(h *telegramHarness) {
			for i := range 12 {
				h.attempt(fmt.Sprintf("a%d", i), "failed", "", fmt.Sprintf("Reason %c", 'A'+i), at(16, i))
			}
		}, "payment.failed", "failed", "❌ " + telegramHead + "\n12 failed attempts" +
			"\n1× Reason L · 16:11\n1× Reason K · 16:10\n1× Reason J · 16:09\n1× Reason I · 16:08\n1× Reason H · 16:07" +
			"\n1× Reason G · 16:06\n1× Reason F · 16:05\n1× Reason E · 16:04\n1× Reason D · 16:03\n1× Reason C · 16:02" +
			"\n… 2 more reasons"},
		"failure with no attempt on record": {func(*telegramHarness) {}, "payment.failed", "failed",
			"❌ " + telegramHead + "\nFailed"},
	} {
		t.Run(name, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				h := newTelegramHarness(t, db, 0)
				tc.setup(h)
				wantCalls(t, h.deliver(tc.typ, tc.status), telegramCall{Method: "sendMessage", Silent: true, Text: tc.want})
			})
		})
	}
}

// One checkout attempt can hold several provider tries; each failed try counts.
func TestTelegramPaymentMessageCountsProviderTries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.attempt("a1", "captured", "", "", at(16, 9))
		h.write(func(tx store.Tx) error {
			for id, minute := range map[string]int{"pay_1": 6, "pay_2": 7} {
				if err := store.UpsertProviderPaymentAttempt(ctx, tx, store.ProviderPaymentAttempt{ProviderAccountID: "pa_1",
					ProviderPaymentID: id, AttemptID: "a1", Status: "failed", FailureMessage: "Insufficient funds",
					CreatedAt: at(16, minute), UpdatedAt: at(16, 30)}); err != nil {
					return err
				}
			}
			return store.UpsertProviderPaymentAttempt(ctx, tx, store.ProviderPaymentAttempt{ProviderAccountID: "pa_1",
				ProviderPaymentID: "pay_3", AttemptID: "a1", Status: "captured", CreatedAt: at(16, 8), UpdatedAt: at(16, 30)})
		})
		wantCalls(t, h.deliver("payment.succeeded", "succeeded"), telegramCall{Method: "sendMessage", Silent: true,
			Text: "✅ " + telegramHead + "\nPaid 16:09 UTC on attempt 3\n2× Insufficient funds · last 16:07"})
	})
}

func TestTelegramPaymentMessageGoneIsPostedAgain(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.deliver("payment.created", "created")

		h.tg.override = func(method string) (int, string, bool) {
			if method == "editMessageText" {
				return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message to edit not found"}`, true
			}
			return 0, "", false
		}
		wantCalls(t, h.deliver("payment.processing", "processing"),
			telegramCall{Method: "editMessageText", MessageID: 321, Text: "⏳ " + telegramHead},
			telegramCall{Method: "sendMessage", Silent: true, Text: "⏳ " + telegramHead})

		h.tg.override = nil
		wantCalls(t, h.deliver("payment.failed", "failed"),
			telegramCall{Method: "editMessageText", MessageID: 322, Text: "❌ " + telegramHead + "\nFailed"})
	})
}

func TestTelegramPaymentMessageUnchangedIsNotAnError(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.deliver("payment.created", "created")
		h.tg.override = func(method string) (int, string, bool) {
			return http.StatusBadRequest, `{"ok":false,"description":"Bad Request: message is not modified: specified new message content is exactly the same"}`, true
		}
		// A job that failed would come back after its backoff; this one must not.
		wantCalls(t, h.deliver("payment.failed", "failed"),
			telegramCall{Method: "editMessageText", MessageID: 321, Text: "❌ " + telegramHead + "\nFailed"})
		h.e.clk.Add(time.Hour)
		h.run(0)
	})
}

func TestTelegramPaymentMessageRateLimitRetries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.deliver("payment.created", "created")
		h.tg.override = func(method string) (int, string, bool) {
			return http.StatusTooManyRequests, `{"ok":false,"description":"Too Many Requests"}`, true
		}
		wantCalls(t, h.deliver("payment.failed", "failed"),
			telegramCall{Method: "editMessageText", MessageID: 321, Text: "❌ " + telegramHead + "\nFailed"})
		h.tg.override = nil
		h.e.clk.Add(time.Hour)
		h.run(1)
		wantCalls(t, h.tg.take(),
			telegramCall{Method: "editMessageText", MessageID: 321, Text: "❌ " + telegramHead + "\nFailed"})
	})
}

// Workers run jobs at once: two events of one payment must not post twice.
func TestTelegramPaymentConcurrentEventsPostOneMessage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.emit("payment.created", "failed")
		h.emit("payment.failed", "failed")
		h.run(2)

		h.e.clk.Add(time.Hour) // past the waiting job's backoff and any lock
		if _, err := h.q.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}

		var sends int
		for _, c := range h.tg.take() {
			if c.Method == "sendMessage" {
				sends++
			}
		}
		if sends != 1 {
			t.Fatalf("sendMessage calls = %d, want 1", sends)
		}
	})
}

// A job that finds the message locked waits: it sends nothing, so its retry
// cannot repeat the email.
func TestTelegramPaymentLockedMessageMakesTheJobWait(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		key := store.TelegramPaymentMessageKey{OrgID: "org_1", Mode: "test", ChatID: "-100123", PaymentID: "p123"}
		now := h.e.clk.Now()
		h.write(func(tx store.Tx) error {
			_, locked, err := store.LockTelegramPaymentMessage(ctx, tx, key, now, now.Add(time.Hour))
			if err == nil && !locked {
				t.Fatal("first lock refused")
			}
			return err
		})

		h.emit("payment.created", "created")
		h.run(1)
		if calls := h.tg.take(); len(calls) != 0 || len(h.mailer.messages) != 0 {
			t.Fatalf("locked job sent: telegram %+v, email %d", calls, len(h.mailer.messages))
		}
		var lastError string
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT COALESCE(last_error,'') FROM jobs WHERE kind = ?`, outbound.JobNotify).Scan(&lastError)
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lastError, "another job") {
			t.Fatalf("job error = %q", lastError)
		}

		h.e.clk.Add(2 * time.Hour)
		h.run(1)
		wantCalls(t, h.tg.take(), telegramCall{Method: "sendMessage", Silent: true, Text: "⏳ " + telegramHead})
		if len(h.mailer.messages) != 1 {
			t.Fatalf("emails after retry = %d", len(h.mailer.messages))
		}
	})
}

// A job that outlived its lock must not free or overwrite the next job's.
func TestTelegramPaymentExpiredLockCannotUnlockTheNextOne(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		key := store.TelegramPaymentMessageKey{OrgID: "org_1", Mode: "test", ChatID: "-100123", PaymentID: "p123"}
		lock := func(now time.Time) (int64, bool) {
			var id int64
			var locked bool
			h.write(func(tx store.Tx) (err error) {
				id, locked, err = store.LockTelegramPaymentMessage(ctx, tx, key, now, now.Add(2*time.Minute))
				return err
			})
			return id, locked
		}
		first := at(16, 0)
		lock(first)
		second := first.Add(3 * time.Minute)
		if _, locked := lock(second); !locked {
			t.Fatal("expired lock was not taken over")
		}
		h.write(func(tx store.Tx) error {
			return store.UnlockTelegramPaymentMessage(ctx, tx, key, 999, first.Add(2*time.Minute))
		})
		if _, locked := lock(second.Add(time.Second)); locked {
			t.Fatal("the stale unlock freed the second job's lock")
		}
		h.write(func(tx store.Tx) error {
			return store.UnlockTelegramPaymentMessage(ctx, tx, key, 777, second.Add(2*time.Minute))
		})
		if id, locked := lock(second.Add(time.Second)); !locked || id != 777 {
			t.Fatalf("after the real unlock: id %d, locked %v", id, locked)
		}
	})
}

// The request URL holds the bot token, and job errors are logged and stored.
func TestTelegramDeleteFailureKeepsTheTokenOutOfTheJobError(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 1)
		h.deliver("payment.created", "created")
		h.tg.failWith = errors.New("connection refused")
		h.e.clk.Add(time.Hour)
		h.run(1)
		var lastError string
		if err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT COALESCE(last_error,'') FROM jobs WHERE kind = ?`, outbound.JobDeleteTelegram).Scan(&lastError)
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lastError, "connection refused") || strings.Contains(lastError, "ABC_def-789") {
			t.Fatalf("job error = %q", lastError)
		}
	})
}

func TestTelegramPaymentReviewEventStaysSeparate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		h.deliver("payment.created", "created")
		calls := h.deliver("payment.needs_review", "needs_review")
		if len(calls) != 1 || calls[0].Method != "sendMessage" || !strings.HasPrefix(calls[0].Text, "[TEST] payment.needs_review · p123") {
			t.Fatalf("review alert: %+v", calls)
		}
		wantCalls(t, h.deliver("payment.failed", "needs_review"),
			telegramCall{Method: "editMessageText", MessageID: 321, Text: "⚠️ " + telegramHead + "\nNeeds review"})
	})
}

// After its retention delete the message is forgotten, so no edit is wasted.
func TestTelegramPaymentMessageForgottenAfterRetentionDelete(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 1)
		h.deliver("payment.created", "created")
		h.e.clk.Add(time.Hour)
		h.run(1)
		wantCalls(t, h.tg.take(), telegramCall{Method: "deleteMessage", MessageID: 321})

		wantCalls(t, h.deliver("payment.failed", "failed"),
			telegramCall{Method: "sendMessage", Silent: true, Text: "❌ " + telegramHead + "\nFailed"})
	})
}

func TestTelegramNonPaymentEventKeepsItsMessage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newTelegramHarness(t, db, 0)
		cfg := outbound.NotificationConfig{TelegramChatID: "-100123", TelegramEnabled: true, EventTypes: []string{"dispute.opened"}}
		if err := h.svc.SaveNotificationConfig(ctx, "org_1", "test", cfg, ""); err != nil {
			t.Fatal(err)
		}
		h.write(func(tx store.Tx) error {
			return h.svc.Emit(ctx, tx, events.Event{OrgID: "org_1", AppID: "app_1", Mode: "test", Type: "dispute.opened",
				ObjectType: "dispute", ObjectID: "dp_1", ObjectVersion: 1, Data: map[string]any{"id": "dp_1"}})
		})
		h.run(1)
		wantCalls(t, h.tg.take(), telegramCall{Method: "sendMessage", Silent: true, Text: "[TEST] dispute.opened · dp_1"})
	})
}
