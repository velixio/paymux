package dblog_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/dblog"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/redact"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

type env struct {
	rec    *dblog.Recorder
	log    *slog.Logger
	stdout *bytes.Buffer
	db     *store.DB
	red    *redact.Redactor
	sb     *syncBuf
}

func newEnv(t *testing.T, db *store.DB, mut func(*dblog.Options)) *env {
	t.Helper()
	l, err := db.OpenLogs(context.Background(), migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if l != db {
		t.Cleanup(func() { l.Close() })
	}
	e := &env{stdout: &bytes.Buffer{}, db: l, red: redact.New()}
	e.sb = &syncBuf{b: e.stdout}
	o := dblog.Options{DB: l, Redactor: e.red, Stdout: slog.New(slog.NewTextHandler(e.sb, nil)), FlushEvery: 5 * time.Millisecond}
	if mut != nil {
		mut(&o)
	}
	e.rec = dblog.New(o)
	e.rec.Start(context.Background())
	t.Cleanup(func() { e.rec.Close() })
	e.log = slog.New(e.rec.Handler(slog.NewJSONHandler(&syncBuf{b: &bytes.Buffer{}}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return e
}

type syncBuf struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

func (e *env) out() string { e.sb.mu.Lock(); defer e.sb.mu.Unlock(); return e.stdout.String() }

func (e *env) rows(t *testing.T) []store.LogRow {
	t.Helper()
	e.rec.Flush(context.Background())
	var out []store.LogRow
	if err := e.db.Read(context.Background(), func(q store.Querier) (err error) {
		out, err = store.SearchLogs(context.Background(), q, store.LogFilter{WithBodies: true, Limit: 1000})
		return
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOnlyWarnAndErrorAreStored(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		e.log.Debug("debug line")
		e.log.Info("info line")
		e.log.Warn("warn line")
		e.log.Error("error line")
		rows := e.rows(t)
		if len(rows) != 2 {
			t.Fatalf("stored %d rows: %+v", len(rows), rows)
		}
		levels := map[string]string{rows[0].Level: rows[0].Msg, rows[1].Level: rows[1].Msg}
		if levels["warn"] != "warn line" || levels["error"] != "error line" {
			t.Errorf("%v", levels)
		}
		for _, r := range rows {
			if r.Kind != "app" || r.ID == "" || r.At.IsZero() {
				t.Errorf("%+v", r)
			}
		}
	})
}

func TestLevelCanBeLoweredExplicitly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(o *dblog.Options) { o.Level = slog.LevelInfo })
		e.log.Debug("debug line")
		e.log.Info("info line")
		if rows := e.rows(t); len(rows) != 1 || rows[0].Level != "info" {
			t.Errorf("%+v", rows)
		}
		e.rec.SetLevel(slog.LevelError)
		e.log.Warn("not stored now")
		if rows := e.rows(t); len(rows) != 1 {
			t.Errorf("after raising the level: %+v", rows)
		}
	})
}

func TestIdentifiersBecomeColumnsAndIDs(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		ctx := logctx.With(context.Background(), "request_id", "req_ctx", "org_id", "org_1")
		e.log.With("component", "checkout").ErrorContext(ctx, "capture failed",
			"payment_id", "pay_1", "attempt_id", "att_1", "refund_id", "rfd_1", "subscription_id", "sub_1", "mandate_id", "mdt_1",
			"payout_id", "pot_1", "invoice_id", "inv_1", "provider_account", "pa_1", "connector", "razorpay",
			"provider_order_id", "order_9", "provider_payment_id", "pay_prov_9", "provider_request_id", "preq_9", "event_id", "evt_1",
			"job_id", "job_1", "job_kind", "subscription.charge", "app_id", "app_1", "status", 502, "attempt", 3)
		r := e.rows(t)[0]
		if r.Component != "checkout" || r.RequestID != "req_ctx" || r.OrgID != "org_1" || r.AppID != "app_1" || r.PaymentID != "pay_1" ||
			r.AttemptID != "att_1" || r.RefundID != "rfd_1" || r.SubscriptionID != "sub_1" || r.ProviderAccountID != "pa_1" ||
			r.Connector != "razorpay" || r.JobID != "job_1" {
			t.Errorf("columns: %+v", r)
		}
		for _, want := range []string{`"mandate_id":"mdt_1"`, `"payout_id":"pot_1"`, `"invoice_id":"inv_1"`, `"provider_order_id":"order_9"`,
			`"provider_payment_id":"pay_prov_9"`, `"provider_request_id":"preq_9"`, `"webhook_event_id":"evt_1"`, `"job_kind":"subscription.charge"`} {
			if !strings.Contains(r.IDs, want) {
				t.Errorf("ids lack %s: %s", want, r.IDs)
			}
		}
		if !strings.Contains(r.Attrs, `"status":502`) || !strings.Contains(r.Attrs, `"attempt":3`) {
			t.Errorf("attrs: %s", r.Attrs)
		}
		if strings.Contains(r.Attrs, "payment_id") {
			t.Errorf("identifiers are not repeated in attrs: %s", r.Attrs)
		}
	})
}

func TestRecordsAreRedacted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		e.red.SetKnown("account:pa_1", "KNOWN-SECRET-VALUE-1234")
		e.log.With("group_attr", slog.GroupValue(slog.String("token", "tok_abcdefghijkl"), slog.String("note", "KNOWN-SECRET-VALUE-1234"))).
			Error("call failed for asha@example.com with KNOWN-SECRET-VALUE-1234",
				"password", "hunter2hunter2", "customer_email", "asha@example.com", "phone", "9876543210", "customer_name", "Asha Rao",
				"err", errors.New("upstream said: api_key=ZZZZZZZZZZ KNOWN-SECRET-VALUE-1234"), "payment_id", "pay_1",
				slog.Group("req", slog.String("authorization", "Bearer abcdefghijkl"), slog.String("vpa", "asha@okbank")))
		r := e.rows(t)[0]
		all := strings.Join([]string{r.Msg, r.Attrs, r.IDs, r.Component}, "\n")
		for _, leak := range []string{"KNOWN-SECRET", "hunter2", "asha@", "9876543210", "Asha Rao", "ZZZZZZZZZZ", "tok_abc", "abcdefghijkl", "okbank"} {
			if strings.Contains(all, leak) {
				t.Errorf("row leaks %q:\n%s", leak, all)
			}
		}
		if r.PaymentID != "pay_1" {
			t.Errorf("ids stay: %+v", r)
		}
	})
}

type panicky struct{}

func (panicky) String() string { panic("boom") }

func TestAHostileValueNeverBreaksLogging(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		e.log.Error("with a panicking value", "v", panicky{})
		e.log.Error("after it", "k", "v")
		rows := e.rows(t)
		found := false
		for _, r := range rows {
			if r.Msg == "after it" {
				found = true
			}
		}
		if !found {
			t.Errorf("logging stopped working: %+v", rows)
		}
	})
}

func TestComponentDefaultsToTheCallingPackage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		e.log.Error("from a test")
		if c := e.rows(t)[0].Component; c != "internal/dblog_test" && c != "internal/dblog" {
			t.Errorf("component = %q", c)
		}
	})
}

func TestQueueOverflowNeverBlocksAndIsCounted(t *testing.T) {
	block, started := make(chan struct{}), make(chan struct{}, 1)
	var once sync.Once
	var written int
	var mu sync.Mutex
	e := newEnv(t, storetest.SQLite(t), func(o *dblog.Options) {
		o.QueueSize, o.BatchSize = 8, 4
		o.ReportEvery = 20 * time.Millisecond
		o.Write = func(_ context.Context, rows []store.LogRow) error {
			once.Do(func() { started <- struct{}{} })
			<-block
			mu.Lock()
			written += len(rows)
			mu.Unlock()
			return nil
		}
	})
	e.log.Error("first")
	<-started // the writer is now stuck inside a write
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			e.log.Error("flood")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("logging blocked on a full queue")
	}
	if d := e.rec.Stats().Dropped; d < 400 {
		t.Errorf("dropped = %d", d)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(e.out(), "dropped") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(e.out(), "dropped") {
		t.Errorf("no periodic 'dropped N' line on stdout: %q", e.out())
	}
	close(block)
}

func TestWriteFailuresAreCountedAndNeverRecurse(t *testing.T) {
	e := newEnv(t, storetest.SQLite(t), func(o *dblog.Options) {
		o.Write = func(context.Context, []store.LogRow) error { return errors.New("disk full") }
	})
	for i := 0; i < 20; i++ {
		e.log.Error("will not be stored")
		time.Sleep(time.Millisecond)
	}
	e.rec.Flush(context.Background())
	if f := e.rec.Stats().Failed; f == 0 {
		t.Error("failures are not counted")
	}
	if !strings.Contains(e.out(), "disk full") {
		t.Errorf("the write error goes to stdout: %q", e.out())
	}
	// The failure line was written to stdout only: it is not itself a stored row.
	if s := e.rec.Stats(); s.Stored != 0 {
		t.Errorf("stored = %d", s.Stored)
	}
}

func TestCloseFlushesQueuedRows(t *testing.T) {
	db := storetest.SQLite(t)
	if err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, db, func(o *dblog.Options) { o.FlushEvery = time.Hour })
	for i := 0; i < 30; i++ {
		e.log.Error("queued")
	}
	e.rec.Close()
	var n int
	_ = e.db.Read(context.Background(), func(q store.Querier) (err error) { n, err = store.CountLogs(context.Background(), q); return })
	if n != 30 {
		t.Errorf("stored %d of 30", n)
	}
	e.log.Error("after close") // must not panic or block
}

func TestHTTPRowsOnlyForServerErrorsAndRejectedAPIRequests(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		ctx := logctx.With(context.Background(), "request_id", "req_77", "app_id", "app_1")
		for _, c := range []struct {
			method, route string
			status        int
			want          string
		}{
			{"GET", "GET /pay/{id}", 200, ""},
			{"GET", "GET /pay/{id}", 302, ""},
			{"GET", "GET /pay/{id}", 404, ""},
			{"POST", "POST /dashboard/login", 401, ""},
			{"POST", "/v1/", 200, ""},
			{"POST", "/v1/", 401, "warn"},
			{"POST", "/v1/", 422, "warn"},
			{"POST", "POST /hooks/{provider_account_id}", 400, "warn"},
			{"GET", "GET /pay/{id}", 500, "error"},
			{"POST", "/v1/", 503, "error"},
			{"GET", "unmatched", 502, "error"},
		} {
			e.rec.HTTP(ctx, dblog.HTTPEvent{Method: c.method, Route: c.route, Status: c.status, Duration: 12 * time.Millisecond})
			rows := e.rows(t)
			if c.want == "" {
				if len(rows) != 0 {
					t.Fatalf("%s %d stored %+v", c.route, c.status, rows)
				}
				continue
			}
			if len(rows) != 1 || rows[0].Level != c.want || rows[0].Kind != "http" || rows[0].Status != c.status || rows[0].RequestID != "req_77" || rows[0].AppID != "app_1" {
				t.Fatalf("%s %d: %+v", c.route, c.status, rows)
			}
			if err := e.db.Write(context.Background(), func(tx store.Tx) error {
				_, err := tx.ExecContext(context.Background(), "DELETE FROM logs")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func providerCall() connector.CallRecord {
	return connector.CallRecord{
		Method: "POST", URL: url.URL{Scheme: "https", Host: "api.example-psp.com", Path: "/v1/payments/pay_prov_1/capture",
			RawQuery: "api_key=QQQQQQQQQQ&hash=HHHHHHHHHH&code=CCCCCCCCCC&order=ord_1"},
		Status: 502, Duration: 1500 * time.Millisecond, Failure: connector.FailStatus, Attempt: 1,
		RequestHeader:  http.Header{"Content-Type": {"application/json"}, "X-Api-Key": {"HDRKEY123456"}},
		ResponseHeader: http.Header{"X-Request-Id": {"prov_req_42"}, "Set-Cookie": {"sid=SESSION123456"}},
		RequestBody:    []byte(`{"amount":49900,"customer":{"email":"asha@example.com","name":"Asha Rao"},"key_secret":"KNOWN-SECRET-VALUE-1234"}`),
		ResponseBody:   []byte(`{"error":{"message":"bad gateway for asha@example.com","debug":"auth KNOWN-SECRET-VALUE-1234"}}`),
	}
}

func TestProviderCallRow(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		e.red.SetKnown("account:pa_1", "KNOWN-SECRET-VALUE-1234")
		ctx := logctx.With(context.Background(), "request_id", "req_1", "payment_id", "pay_1", "attempt_id", "att_1")
		e.rec.ProviderCall(dblog.ProviderMeta{Connector: "razorpay", AccountID: "pa_1", OrgID: "org_1"})(ctx, providerCall())
		r := e.rows(t)[0]
		if r.Level != "error" || r.Kind != "provider" || r.Connector != "razorpay" || r.ProviderAccountID != "pa_1" || r.OrgID != "org_1" ||
			r.PaymentID != "pay_1" || r.AttemptID != "att_1" || r.RequestID != "req_1" || r.Status != 502 || r.DurationMS != 1500 {
			t.Errorf("%+v", r)
		}
		if !strings.Contains(r.IDs, `"provider_request_id":"prov_req_42"`) {
			t.Errorf("ids: %s", r.IDs)
		}
		if !strings.Contains(r.Msg, "502") || !strings.Contains(r.Msg, "api.example-psp.com") || !strings.Contains(r.Msg, "/capture") {
			t.Errorf("msg: %s", r.Msg)
		}
		all := strings.Join([]string{r.Msg, r.Attrs, r.IDs, r.Request, r.Response}, "\n")
		for _, leak := range []string{"QQQQQQQQQQ", "HHHHHHHHHH", "CCCCCCCCCC", "HDRKEY123456", "SESSION123456", "KNOWN-SECRET", "asha@", "Asha Rao"} {
			if strings.Contains(all, leak) {
				t.Errorf("provider row leaks %q:\n%s", leak, all)
			}
		}
		for _, keep := range []string{"order=ord_1", `"amount":49900`, "bad gateway", "application/json"} {
			if !strings.Contains(all, keep) {
				t.Errorf("provider row lost %q:\n%s", keep, all)
			}
		}
		if !strings.Contains(r.Attrs, `"failure":"status"`) {
			t.Errorf("attrs: %s", r.Attrs)
		}
	})
}

func TestProviderBodiesAreCapped(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		c := providerCall()
		c.ResponseBody = []byte(`{"data":"` + strings.Repeat("y", 40000) + `"}`)
		c.RequestBody = []byte(strings.Repeat("z", 40000))
		e.rec.ProviderCall(dblog.ProviderMeta{Connector: "payu", AccountID: "pa_2"})(context.Background(), c)
		r := e.rows(t)[0]
		if len(r.Request) > dblog.BodyCap+2048 || len(r.Response) > dblog.BodyCap+2048 {
			t.Errorf("sizes %d %d", len(r.Request), len(r.Response))
		}
		if !strings.Contains(r.Response, "truncated") {
			t.Errorf("no truncation mark: %.100s", r.Response)
		}
	})
}

func TestProviderTransportErrorAndRetry(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		e.red.SetKnown("account:pa_1", "KNOWN-SECRET-VALUE-1234")
		call := e.rec.ProviderCall(dblog.ProviderMeta{Connector: "zoho", AccountID: "pa_1"})
		c := providerCall()
		c.Status, c.Failure, c.ResponseBody, c.RequestBody = 0, connector.FailTimeout, nil, nil
		c.Err = errors.New(`Post "https://api.example-psp.com/x?api_key=QQQQQQQQQQ": context deadline exceeded KNOWN-SECRET-VALUE-1234`)
		call(context.Background(), c)
		retry := providerCall()
		retry.WillRetry = true
		retry.Status = 503
		call(context.Background(), retry)
		rows := e.rows(t)
		if len(rows) != 2 {
			t.Fatalf("%+v", rows)
		}
		for _, r := range rows {
			if strings.Contains(r.Msg+r.Attrs, "KNOWN-SECRET") || strings.Contains(r.Msg+r.Attrs, "QQQQQQQQQQ") {
				t.Errorf("leak: %+v", r)
			}
			if r.Status == 503 {
				if r.Level != "warn" || r.Request != "" || r.Response != "" {
					t.Errorf("a retry is a warning without bodies: %+v", r)
				}
			} else if r.Level != "error" || !strings.Contains(r.Attrs, "timeout") {
				t.Errorf("timeout row: %+v", r)
			}
		}
	})
}

func TestShouldRecordHTTP(t *testing.T) {
	for _, c := range []struct {
		route  string
		status int
		level  string
		ok     bool
	}{
		{"GET /x", 200, "", false}, {"GET /x", 404, "", false}, {"GET /x", 500, "error", true}, {"/v1/", 400, "warn", true},
		{"POST /hooks/{provider_account_id}", 401, "warn", true}, {"POST /hooks/{provider_account_id}", 200, "", false},
	} {
		level, ok := dblog.ShouldRecordHTTP(c.route, c.status)
		if ok != c.ok || level != c.level {
			t.Errorf("%s %d: %s %v", c.route, c.status, level, ok)
		}
	}
}
