package outbound_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/outbound/stdwebhooks"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t   *testing.T
	db  *store.DB
	q   *jobs.Queue
	svc *outbound.Service
	clk *clock
	box *secrets.Box
}

func newEnv(t *testing.T, db *store.DB, allowPrivate bool) *env {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	box, err := secrets.NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.UnixMilli(1_800_000_000_000).UTC()}
	// An endpoint belongs to an app that exists: org_1 owns app_1 and app_2.
	err = db.Write(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO orgs (id, name, created_at) VALUES ('org_1', 'Acme', 1)`); err != nil {
			return err
		}
		for _, id := range []string{"app_1", "app_2"} {
			if err := store.InsertApp(ctx, tx, store.App{ID: id, OrgID: "org_1", Name: id, Slug: id, CreatedAt: clk.t}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	q := jobs.New(db, jobs.Options{Clock: clk.Now, Logger: slog.New(slog.DiscardHandler)})
	svc := outbound.New(outbound.Options{DB: db, Queue: q, Box: box, AllowPrivate: allowPrivate, Now: clk.Now,
		Logger: slog.New(slog.DiscardHandler)})
	svc.Register()
	return &env{t: t, db: db, q: q, svc: svc, clk: clk, box: box}
}

func (e *env) endpoint(app, url string, types ...string) (id, secret string) {
	e.t.Helper()
	ep, secret, err := e.svc.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: "org_1", AppID: app, URL: url, EventTypes: types, Mode: "test"})
	if err != nil {
		e.t.Fatal(err)
	}
	return ep.ID, secret
}

func (e *env) emit(app, typ string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return e.svc.Emit(ctx, tx, events.Event{OrgID: "org_1", AppID: app, Mode: "test", Type: typ,
			ObjectType: "payment", ObjectID: "pay_1", ObjectVersion: 2, Data: map[string]any{"id": "pay_1", "version": 2}})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) })
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) str(query string, args ...any) string {
	e.t.Helper()
	var s string
	err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&s) })
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// run executes all due jobs until none remain, returning how many ran.
func (e *env) run() int {
	e.t.Helper()
	total := 0
	for {
		n, err := e.q.RunOnce(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
}

type recorded struct {
	Header http.Header
	Body   []byte
}

type sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	got  []recorded
	code int
}

func newSink(t *testing.T, code int) *sink {
	s := &sink{code: code}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.got = append(s.got, recorded{r.Header.Clone(), b})
		c := s.code
		s.mu.Unlock()
		w.WriteHeader(c)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) hits() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.got...)
}
func (s *sink) setCode(c int) { s.mu.Lock(); s.code = c; s.mu.Unlock() }

func TestEmitFansOutToMatchingEnabledEndpoints(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		all, listed, other, off, otherApp := newSink(t, 200), newSink(t, 200), newSink(t, 200), newSink(t, 200), newSink(t, 200)
		e.endpoint("app_1", all.srv.URL)
		e.endpoint("app_1", listed.srv.URL, "refund.created", "payment.succeeded")
		e.endpoint("app_1", other.srv.URL, "refund.created")
		offID, _ := e.endpoint("app_1", off.srv.URL)
		if err := e.svc.DisableEndpoint(ctx, offID); err != nil {
			t.Fatal(err)
		}
		e.endpoint("app_2", otherApp.srv.URL)

		e.emit("app_1", "payment.succeeded")
		if n := e.count(`SELECT COUNT(*) FROM events`); n != 1 {
			t.Fatalf("events=%d", n)
		}
		if n := e.count(`SELECT COUNT(*) FROM webhook_deliveries`); n != 2 {
			t.Fatalf("deliveries=%d", n)
		}
		if n := e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'webhook.deliver'`); n != 2 {
			t.Fatalf("jobs=%d", n)
		}
		e.run()
		for name, s := range map[string]*sink{"all": all, "listed": listed} {
			if len(s.hits()) != 1 {
				t.Errorf("%s hits=%d", name, len(s.hits()))
			}
		}
		for name, s := range map[string]*sink{"other": other, "off": off, "otherApp": otherApp} {
			if len(s.hits()) != 0 {
				t.Errorf("%s got a delivery", name)
			}
		}
	})
}

func TestEmitIsTransactional(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		s := newSink(t, 200)
		e.endpoint("app_1", s.srv.URL)
		_ = db.Write(ctx, func(tx store.Tx) error {
			if err := e.svc.Emit(ctx, tx, events.Event{AppID: "app_1", Type: "x.y"}); err != nil {
				t.Fatal(err)
			}
			return context.Canceled // roll back
		})
		if e.count(`SELECT COUNT(*) FROM events`)+e.count(`SELECT COUNT(*) FROM webhook_deliveries`)+
			e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'webhook.deliver'`) != 0 {
			t.Fatal("rows leaked from a rolled-back transaction")
		}
	})
}

func TestDeliverSignedStandardWebhook(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		s := newSink(t, 204)
		_, secret := e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "payment.succeeded")
		e.run()

		hits := s.hits()
		if len(hits) != 1 {
			t.Fatalf("hits=%d", len(hits))
		}
		h := hits[0]
		v, _ := stdwebhooks.NewVerifier(secret)
		v.Now = e.clk.Now
		id, err := v.Verify(h.Header, h.Body)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if want := e.str(`SELECT id FROM events`); id != want {
			t.Fatalf("webhook-id %q, want event id %q", id, want)
		}
		if h.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type %q", h.Header.Get("Content-Type"))
		}
		var body struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Data      struct {
				Object map[string]any `json:"object"`
			} `json:"data"`
		}
		if err := json.Unmarshal(h.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Type != "payment.succeeded" || body.Data.Object["id"] != "pay_1" || body.Data.Object["version"] != float64(2) {
			t.Fatalf("body %s", h.Body)
		}
		if ts, err := time.Parse(time.RFC3339, body.Timestamp); err != nil || !ts.Equal(e.clk.Now()) {
			t.Fatalf("timestamp %q (%v)", body.Timestamp, err)
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "delivered" {
			t.Fatalf("status %s", st)
		}
		if e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'webhook.deliver' AND state = 'pending'`) != 0 {
			t.Fatal("job left pending")
		}
	})
}

func TestRetriesFollowDefaultScheduleThenFail(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		s := newSink(t, 500)
		e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "payment.failed")

		delays := []time.Duration{0, time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour,
			6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 24 * time.Hour}
		for i, d := range delays {
			if i > 0 {
				e.clk.Add(d - time.Second)
				if n := e.run(); n != 0 {
					t.Fatalf("attempt %d ran %d jobs one second early", i+1, n)
				}
				e.clk.Add(time.Second)
			}
			if n := e.run(); n != 1 {
				t.Fatalf("attempt %d: ran %d jobs", i+1, n)
			}
			if got := len(s.hits()); got != i+1 {
				t.Fatalf("after attempt %d hits=%d", i+1, got)
			}
			wantStatus := "pending"
			if i == len(delays)-1 {
				wantStatus = "failed"
			}
			if st := e.str(`SELECT status FROM webhook_deliveries`); st != wantStatus {
				t.Fatalf("attempt %d status %s", i+1, st)
			}
		}
		e.clk.Add(7 * 24 * time.Hour)
		if n := e.run(); n != 0 {
			t.Fatalf("job ran after final failure")
		}
		if e.count(`SELECT attempt_count FROM webhook_deliveries`) != 9 ||
			e.count(`SELECT last_status FROM webhook_deliveries`) != 500 {
			t.Fatal("attempt state not recorded")
		}
		if !strings.Contains(e.str(`SELECT last_error FROM webhook_deliveries`), "500") {
			t.Fatal("last_error missing status")
		}
	})
}

func TestRetryScheduleIsConfigurableAndRecovers(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0, 10 * time.Second} }
		s := newSink(t, 503)
		e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "payment.failed")
		e.run()
		s.setCode(200)
		e.clk.Add(10 * time.Second)
		e.run()
		if len(s.hits()) != 2 {
			t.Fatalf("hits=%d", len(s.hits()))
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "delivered" {
			t.Fatalf("status %s", st)
		}
		if e.count(`SELECT attempt_count FROM webhook_deliveries`) != 2 {
			t.Fatal("attempt_count")
		}
	})
}

func TestRedirectsAreFailures(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0} }
		s := newSink(t, 302)
		e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "x.y")
		e.run()
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "failed" {
			t.Fatalf("status %s", st)
		}
	})
}

func TestSSRFBlocksPrivateTargetUnlessAllowed(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, false)
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0} }
		s := newSink(t, 200) // listens on 127.0.0.1
		e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "x.y")
		e.run()
		if len(s.hits()) != 0 {
			t.Fatal("request reached a loopback target")
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "failed" {
			t.Fatalf("status %s", st)
		}
		if !strings.Contains(e.str(`SELECT last_error FROM webhook_deliveries`), "private") {
			t.Fatalf("last_error: %s", e.str(`SELECT last_error FROM webhook_deliveries`))
		}
	})
}

func TestDisabledEndpointAtDeliveryTimeFailsWithoutRequest(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		s := newSink(t, 200)
		id, _ := e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "x.y")
		if err := e.svc.DisableEndpoint(ctx, id); err != nil {
			t.Fatal(err)
		}
		e.run()
		if len(s.hits()) != 0 {
			t.Fatal("delivered to a disabled endpoint")
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "failed" {
			t.Fatalf("status %s", st)
		}
	})
}

func TestReplayDeliveryKeepsWebhookID(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0} }
		s := newSink(t, 500)
		e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "x.y")
		e.run()
		delID := e.str(`SELECT id FROM webhook_deliveries`)
		if err := e.svc.ReplayDelivery(ctx, delID); err != nil {
			t.Fatal(err)
		}
		s.setCode(200)
		e.run()
		hits := s.hits()
		if len(hits) != 2 {
			t.Fatalf("hits=%d", len(hits))
		}
		if hits[0].Header.Get("webhook-id") != hits[1].Header.Get("webhook-id") || hits[0].Header.Get("webhook-id") == "" {
			t.Fatalf("webhook-id changed: %q vs %q", hits[0].Header.Get("webhook-id"), hits[1].Header.Get("webhook-id"))
		}
		if st := e.str(`SELECT status FROM webhook_deliveries`); st != "delivered" {
			t.Fatalf("status %s", st)
		}
		if e.count(`SELECT attempt_count FROM webhook_deliveries`) != 1 {
			t.Fatal("replay should restart the attempt count")
		}
		// a delivery that is still pending cannot be replayed on top of itself
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0, time.Hour} }
		s.setCode(500)
		e.emit("app_1", "x.z")
		e.run()
		pending := e.str(`SELECT id FROM webhook_deliveries WHERE status = 'pending'`)
		if err := e.svc.ReplayDelivery(ctx, pending); err == nil {
			t.Fatal("replay of a pending delivery accepted")
		}
		if err := e.svc.ReplayDelivery(ctx, "whd_missing"); err == nil {
			t.Fatal("replay of a missing delivery accepted")
		}
	})
}

func TestReplayFailedSince(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, true)
		e.svc.RetrySchedule = func() []time.Duration { return []time.Duration{0} }
		s := newSink(t, 500)
		epID, _ := e.endpoint("app_1", s.srv.URL)
		e.emit("app_1", "a.old")
		e.run()
		e.clk.Add(2 * time.Hour)
		cutoff := e.clk.Now()
		e.emit("app_1", "a.new1")
		e.emit("app_1", "a.new2")
		e.run()
		s.setCode(200)
		n, err := e.svc.ReplayFailedSince(ctx, epID, cutoff)
		if err != nil || n != 2 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		e.run()
		if got := e.count(`SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'delivered'`); got != 2 {
			t.Fatalf("delivered=%d", got)
		}
		if got := e.count(`SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'failed'`); got != 1 {
			t.Fatalf("still failed=%d", got)
		}
	})
}
