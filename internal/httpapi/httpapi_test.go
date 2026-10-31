package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/httpapi"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/server"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

// fixtureHook, when a test sets it, is the server's OnResponse hook.
var fixtureHook server.ResponseHook

type fixture struct {
	t        *testing.T
	db       *store.DB
	svc      *core.Service
	srv      *httptest.Server
	app      store.App
	set      *settings.Service
	testKey  string
	liveKey  string
	otherKey string // test key of another app
	pa       string
}

func newFixture(t *testing.T, db *store.DB) *fixture { return newFixtureRouter(t, db, nil) }

func newFixtureRouter(t *testing.T, db *store.DB, router core.Router) *fixture {
	return newFixtureOpts(t, db, router, nil)
}

// newFixtureOpts builds the fixture; mod may adjust the API options. The
// rate limit is lifted unless mod sets it, so ordinary tests can burst.
func newFixtureOpts(t *testing.T, db *store.DB, router core.Router, mod func(*httpapi.Options)) *fixture {
	t.Helper()
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: jobs.New(db, jobs.Options{}), Router: router})
	f := &fixture{t: t, db: db, svc: svc, set: set, pa: "pa_1"}
	err := db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "test",
			Connector: "fake", Name: "Fake", Status: "active", CreatedAt: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	f.app, err = svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "One", Slug: "one"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "Two", Slug: "two"})
	if err != nil {
		t.Fatal(err)
	}
	f.testKey, _, _ = svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, f.app.ID, "test")
	f.liveKey, _, _ = svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, f.app.ID, "live")
	f.otherKey, _, _ = svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, other.ID, "test")

	box, err := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://pay.example.com")
	opts := httpapi.Options{Core: svc, DB: db, Box: box, Settings: set, BaseURL: base, RatePerSecond: 100000}
	if mod != nil {
		mod(&opts)
	}
	api, err := httpapi.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Options{OnResponse: fixtureHook})
	api.Register(s)
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

type resp struct {
	status int
	header http.Header
	raw    []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.raw, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.raw)
	}
	return m
}

func (r resp) errType(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error object: %s", r.raw)
	}
	if e["message"] == "" || e["request_id"] != r.header.Get("X-Request-Id") || e["request_id"] == "" {
		t.Fatalf("incomplete error body: %s (X-Request-Id=%s)", r.raw, r.header.Get("X-Request-Id"))
	}
	return e["type"].(string)
}

func (f *fixture) do(method, path, key string, body any, hdr ...string) resp {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, raw}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func each(t *testing.T, fn func(t *testing.T, f *fixture)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newFixture(t, db)) })
}

var createBody = map[string]any{"amount": 49900, "currency": "INR", "order_ref": "ord-1", "description": "Pro",
	"metadata": map[string]string{"plan": "pro"}, "customer": map[string]any{"external_ref": "u1", "email": "a@example.com"}}

func (f *fixture) createPayment(body any, hdr ...string) (resp, map[string]any) {
	f.t.Helper()
	r := f.do("POST", "/v1/payments", f.testKey, body, hdr...)
	if r.status != 201 {
		f.t.Fatalf("create: %d %s", r.status, r.raw)
	}
	return r, r.json(f.t)
}

func TestAuthentication(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		for name, hdr := range map[string][]string{
			"no header":    nil,
			"wrong scheme": {"Authorization", "Basic abc"},
			"garbage":      {"Authorization", "Bearer nope"},
			"unknown key":  {"Authorization", "Bearer pmx_test_sk_" + strings.Repeat("a", 43)},
		} {
			r := f.do("GET", "/v1/payments", "", nil, hdr...)
			if r.status != 401 || r.errType(t) != "authentication_error" || r.header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
		}
		if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 200 {
			t.Errorf("valid key: %d %s", r.status, r.raw)
		}
		// Lower-case scheme is accepted.
		if r := f.do("GET", "/v1/payments", "", nil, "Authorization", "bearer "+f.testKey); r.status != 200 {
			t.Errorf("lower-case scheme: %d", r.status)
		}
		_ = f.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE api_keys SET revoked_at = ?", time.Now().UnixMilli())
			return err
		})
		if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 401 {
			t.Errorf("revoked key: %d", r.status)
		}
	})
}

func TestCreatePaymentResponse(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		r, m := f.createPayment(createBody)
		id, _ := m["id"].(string)
		if !regexp.MustCompile(`^[0-9A-Za-z]{14}$`).MatchString(id) || m["object"] != "payment" || m["status"] != "created" || m["amount"] != float64(49900) ||
			m["currency"] != "INR" || m["order_ref"] != "ord-1" || m["version"] != float64(1) {
			t.Fatalf("body %s", r.raw)
		}
		co, _ := m["checkout_url"].(string)
		re := regexp.MustCompile(`^https://pay\.example\.com/pay/([0-9A-Za-z]{14})\?t=([A-Za-z0-9_-]{43})$`)
		sub := re.FindStringSubmatch(co)
		if sub == nil || sub[1] != id {
			t.Fatalf("checkout_url %q", co)
		}
		if strings.Contains(string(r.raw), "client_secret_hash") {
			t.Error("hash leaked")
		}
		if m["checkout_mode"] != "hosted" {
			t.Errorf("default checkout mode = %v", m["checkout_mode"])
		}
		p, err := f.svc.GetPayment(ctx, core.Scope{AppID: f.app.ID, Mode: "test"}, id)
		if err != nil || !core.VerifyClientSecret(p, sub[2], time.Now()) {
			t.Errorf("checkout secret does not verify: %v", err)
		}
		if p.Mode != "test" {
			t.Errorf("mode %s: payments take the mode of the key", p.Mode)
		}
		// A live key creates a live payment.
		live := f.do("POST", "/v1/payments", f.liveKey, createBody)
		if live.status != 201 {
			t.Fatalf("live create: %d %s", live.status, live.raw)
		}
		lp, _ := f.svc.GetPayment(ctx, core.Scope{AppID: f.app.ID, Mode: "live"}, live.json(t)["id"].(string))
		if lp.Mode != "live" {
			t.Errorf("live payment mode %s", lp.Mode)
		}
	})
}

func TestCreatePaymentValidation(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		for name, body := range map[string]any{
			"string amount":   `{"amount":"100","currency":"INR"}`,
			"float amount":    `{"amount":10.5,"currency":"INR"}`,
			"missing amount":  `{"currency":"INR"}`,
			"zero amount":     `{"amount":0,"currency":"INR"}`,
			"bad currency":    `{"amount":100,"currency":"ZZZ"}`,
			"unknown field":   `{"amount":100,"currency":"INR","amout":1}`,
			"not json":        `{"amount":`,
			"empty body":      ``,
			"array":           `[]`,
			"bad return url":  `{"amount":100,"currency":"INR","return_url":"javascript:alert(1)"}`,
			"non-string meta": `{"amount":100,"currency":"INR","metadata":{"a":1}}`,
		} {
			r := f.do("POST", "/v1/payments", f.testKey, body)
			if r.status != 400 || r.errType(t) != "invalid_request" {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
		}
		big := `{"amount":1,"currency":"INR","description":"` + strings.Repeat("x", 2<<20) + `"}`
		if r := f.do("POST", "/v1/payments", f.testKey, big); r.status != 413 || r.errType(t) != "invalid_request" {
			t.Errorf("oversized body: %d", r.status)
		}
		if f.count("SELECT COUNT(*) FROM payments") != 0 {
			t.Error("invalid requests created payments")
		}
	})
}

func TestGetPaymentIsTenantScoped(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		_, m := f.createPayment(createBody)
		id := m["id"].(string)
		if r := f.do("GET", "/v1/payments/"+id, f.testKey, nil); r.status != 200 || r.json(t)["id"] != id {
			t.Fatalf("own get: %d %s", r.status, r.raw)
		}
		for name, key := range map[string]string{"other app": f.otherKey, "other mode": f.liveKey} {
			if r := f.do("GET", "/v1/payments/"+id, key, nil); r.status != 404 || r.errType(t) != "not_found" {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
			if r := f.do("POST", "/v1/payments/"+id+"/cancel", key, nil); r.status != 404 {
				t.Errorf("%s cancel: %d", name, r.status)
			}
			if r := f.do("POST", "/v1/payments/"+id+"/refunds", key, `{}`); r.status != 404 {
				t.Errorf("%s refund: %d", name, r.status)
			}
			if r := f.do("GET", "/v1/payments", key, nil); r.status != 200 || len(r.json(t)["data"].([]any)) != 0 {
				t.Errorf("%s list leaks: %s", name, r.raw)
			}
		}
		if r := f.do("GET", "/v1/payments/pay_missing", f.testKey, nil); r.status != 404 {
			t.Errorf("missing: %d", r.status)
		}
	})
}

func TestListPaymentsPagination(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		var want []string
		for i := 0; i < 5; i++ {
			_, m := f.createPayment(createBody)
			want = append([]string{m["id"].(string)}, want...)
			time.Sleep(2 * time.Millisecond)
		}
		var got []string
		cursor := ""
		for page := 0; page < 10; page++ {
			path := "/v1/payments?limit=2"
			if cursor != "" {
				path += "&starting_after=" + cursor
			}
			m := f.do("GET", path, f.testKey, nil).json(t)
			if m["object"] != "list" {
				t.Fatalf("list body %v", m)
			}
			for _, d := range m["data"].([]any) {
				got = append(got, d.(map[string]any)["id"].(string))
			}
			if m["has_more"] != true {
				break
			}
			cursor, _ = m["next_cursor"].(string)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v want %v", got, want)
		}
		for _, bad := range []string{"limit=0", "limit=abc", "limit=101", "status=bogus"} {
			if r := f.do("GET", "/v1/payments?"+bad, f.testKey, nil); r.status != 400 {
				t.Errorf("%s: %d", bad, r.status)
			}
		}
		if m := f.do("GET", "/v1/payments?status=succeeded", f.testKey, nil).json(t); len(m["data"].([]any)) != 0 {
			t.Error("status filter ignored")
		}
	})
}

func TestCancelPayment(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		_, m := f.createPayment(createBody)
		id := m["id"].(string)
		r := f.do("POST", "/v1/payments/"+id+"/cancel", f.testKey, nil)
		if r.status != 200 || r.json(t)["status"] != "canceled" {
			t.Fatalf("cancel: %d %s", r.status, r.raw)
		}
		if r := f.do("POST", "/v1/payments/"+id+"/cancel", f.testKey, nil); r.status != 200 {
			t.Errorf("repeat cancel: %d", r.status)
		}
		_, m2 := f.createPayment(createBody)
		f.succeed(m2["id"].(string), 49900)
		if r := f.do("POST", "/v1/payments/"+m2["id"].(string)+"/cancel", f.testKey, nil); r.status != 409 || r.errType(t) != "invalid_state" {
			t.Errorf("cancel paid: %d %s", r.status, r.raw)
		}
	})
}

func (f *fixture) succeed(paymentID string, amount int64) {
	f.t.Helper()
	a, err := f.svc.CreateAttempt(ctx, paymentID, f.pa)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.svc.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: amount, Currency: "INR"}); err != nil {
		f.t.Fatal(err)
	}
}

func TestRefundEndpoints(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		_, m := f.createPayment(createBody)
		id := m["id"].(string)
		if r := f.do("POST", "/v1/payments/"+id+"/refunds", f.testKey, `{"amount":100}`); r.status != 409 || r.errType(t) != "not_refundable" {
			t.Fatalf("unpaid refund: %d %s", r.status, r.raw)
		}
		f.succeed(id, 49900)
		r := f.do("POST", "/v1/payments/"+id+"/refunds", f.testKey, `{"amount":10000,"reason":"requested"}`)
		rm := r.json(t)
		if r.status != 201 || rm["object"] != "refund" || rm["amount"] != float64(10000) || rm["status"] != "pending" ||
			rm["initiated_by"] != "api" || rm["payment_id"] != id || !strings.HasPrefix(rm["id"].(string), "ref_") {
			t.Fatalf("refund: %d %s", r.status, r.raw)
		}
		if r := f.do("POST", "/v1/payments/"+id+"/refunds", f.testKey, `{"amount":40000}`); r.status != 409 || r.errType(t) != "refund_amount_exceeds_balance" {
			t.Errorf("over refund: %d %s", r.status, r.raw)
		}
		if r := f.do("POST", "/v1/payments/"+id+"/refunds", f.testKey, `{"amount":-1}`); r.status != 400 {
			t.Errorf("negative: %d", r.status)
		}
		if r := f.do("POST", "/v1/payments/"+id+"/refunds", f.testKey, `{"bogus":1}`); r.status != 400 {
			t.Errorf("unknown field: %d", r.status)
		}
		full := f.do("POST", "/v1/payments/"+id+"/refunds", f.testKey, nil) // default: the rest
		if full.status != 201 || full.json(t)["amount"] != float64(39900) {
			t.Errorf("default refund: %d %s", full.status, full.raw)
		}
		rid := rm["id"].(string)
		if g := f.do("GET", "/v1/refunds/"+rid, f.testKey, nil); g.status != 200 || g.json(t)["id"] != rid {
			t.Errorf("get refund: %d %s", g.status, g.raw)
		}
		for name, key := range map[string]string{"other app": f.otherKey, "other mode": f.liveKey} {
			if g := f.do("GET", "/v1/refunds/"+rid, key, nil); g.status != 404 {
				t.Errorf("%s: %d", name, g.status)
			}
		}
		if f.count("SELECT COUNT(*) FROM audit_log WHERE action = 'refund.create' AND actor_type = 'api_key'") != 2 {
			t.Error("api refunds must be audited with the key as actor")
		}
	})
}

func TestIdempotency(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		first, m1 := f.createPayment(createBody, "Idempotency-Key", "k-1")
		second, m2 := f.createPayment(createBody, "Idempotency-Key", "k-1")
		if m1["id"] != m2["id"] || string(first.raw) != string(second.raw) || m1["checkout_url"] != m2["checkout_url"] {
			t.Fatalf("replay differs:\n%s\n%s", first.raw, second.raw)
		}
		if second.header.Get("Idempotent-Replay") != "true" || first.header.Get("Idempotent-Replay") != "" {
			t.Error("Idempotent-Replay header wrong")
		}
		if f.count("SELECT COUNT(*) FROM payments") != 1 {
			t.Fatal("replay created a second payment")
		}
		different := map[string]any{"amount": 1, "currency": "INR"}
		r := f.do("POST", "/v1/payments", f.testKey, different, "Idempotency-Key", "k-1")
		if r.status != 409 || r.errType(t) != "idempotency_key_reuse" {
			t.Errorf("different body: %d %s", r.status, r.raw)
		}
		// Same key on another path is a different request.
		if r := f.do("POST", "/v1/payments/"+m1["id"].(string)+"/cancel", f.testKey, nil, "Idempotency-Key", "k-1"); r.status != 409 {
			t.Errorf("same key, other endpoint: %d", r.status)
		}
		// Keys are scoped to the app and mode.
		other := f.do("POST", "/v1/payments", f.otherKey, createBody, "Idempotency-Key", "k-1")
		live := f.do("POST", "/v1/payments", f.liveKey, createBody, "Idempotency-Key", "k-1")
		if other.status != 201 || live.status != 201 || other.json(t)["id"] == m1["id"] || live.json(t)["id"] == m1["id"] {
			t.Errorf("cross-tenant collision: %d %d", other.status, live.status)
		}
		// Without a key, or with a new key, requests are independent.
		_, m3 := f.createPayment(createBody)
		_, m4 := f.createPayment(createBody, "Idempotency-Key", "k-2")
		if m3["id"] == m1["id"] || m4["id"] == m1["id"] || m3["id"] == m4["id"] {
			t.Error("independent requests shared an id")
		}
		// Failed (4xx) responses are replayed too.
		bad1 := f.do("POST", "/v1/payments", f.testKey, `{"amount":0,"currency":"INR"}`, "Idempotency-Key", "k-bad")
		bad2 := f.do("POST", "/v1/payments", f.testKey, `{"amount":0,"currency":"INR"}`, "Idempotency-Key", "k-bad")
		if bad1.status != 400 || bad2.status != 400 || bad2.header.Get("Idempotent-Replay") != "true" {
			t.Errorf("4xx replay: %d %d", bad1.status, bad2.status)
		}
		if r := f.do("POST", "/v1/payments", f.testKey, createBody, "Idempotency-Key", strings.Repeat("k", 300)); r.status != 400 {
			t.Errorf("oversized key: %d", r.status)
		}
		// GET ignores the header.
		if r := f.do("GET", "/v1/payments", f.testKey, nil, "Idempotency-Key", "k-1"); r.status != 200 || r.header.Get("Idempotent-Replay") != "" {
			t.Errorf("GET with key: %d", r.status)
		}
		// The stored response carries a checkout secret: it must be encrypted.
		secret := regexp.MustCompile(`t=([A-Za-z0-9_-]{43})`).FindStringSubmatch(m1["checkout_url"].(string))[1]
		var blob []byte
		_ = f.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT response_enc FROM idempotency_keys WHERE key = 'k-1' AND mode = 'test' AND app_id = ?", f.app.ID).Scan(&blob)
		})
		if len(blob) == 0 || bytes.Contains(blob, []byte(secret)) || bytes.Contains(blob, []byte("checkout_url")) {
			t.Error("idempotent response stored in clear")
		}
	})
}

func TestIdempotencyConcurrentRequestsCreateOnePayment(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		statuses := map[int]int{}
		ids := map[string]bool{}
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := f.do("POST", "/v1/payments", f.testKey, createBody, "Idempotency-Key", "same")
				mu.Lock()
				defer mu.Unlock()
				statuses[r.status]++
				if r.status == 201 {
					ids[r.json(t)["id"].(string)] = true
				}
			}()
		}
		wg.Wait()
		if f.count("SELECT COUNT(*) FROM payments") != 1 || len(ids) != 1 || statuses[201] < 1 || statuses[201]+statuses[409] != 6 {
			t.Fatalf("statuses=%v ids=%v payments=%d", statuses, ids, f.count("SELECT COUNT(*) FROM payments"))
		}
	})
}

func TestUnknownRoutesReturnJSONErrors(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		r := f.do("GET", "/v1/nothing", f.testKey, nil)
		if r.status != 404 || r.errType(t) != "not_found" {
			t.Errorf("%d %s", r.status, r.raw)
		}
		if r := f.do("GET", "/v1/nothing", "", nil); r.status != 401 {
			t.Errorf("unauthenticated unknown route: %d", r.status)
		}
		if got := f.do("GET", "/healthz", "", nil).status; got != 200 {
			t.Errorf("healthz still reachable: %d", got)
		}
	})
}

func TestIdempotencyConflictDoesNotEchoRequestBody(t *testing.T) {
	// The error body of a key-reuse conflict must not echo request bodies.
	each(t, func(t *testing.T, f *fixture) {
		f.createPayment(createBody, "Idempotency-Key", "k-9")
		r := f.do("POST", "/v1/payments", f.testKey, map[string]any{"amount": 7, "currency": "INR", "description": "secret-desc"}, "Idempotency-Key", "k-9")
		if strings.Contains(string(r.raw), "secret-desc") || r.status != 409 {
			t.Errorf("%d %s", r.status, r.raw)
		}
	})
}

type stubRouter func(req core.RouteRequest) error

func (f stubRouter) CheckRoute(_ context.Context, _ store.Queryer, req core.RouteRequest) error {
	return f(req)
}

func TestRoutingFailuresAre422(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		var seen core.RouteRequest
		f := newFixtureRouter(t, db, stubRouter(func(req core.RouteRequest) error {
			seen = req
			switch req.Currency {
			case "USD":
				return core.NewError(core.ErrNoRoute, "no provider account can take USD")
			case "BDT":
				return core.NewError(core.ErrCurrencyNotAccepted, "BDT is not accepted for this app")
			}
			return nil
		}))
		for cur, want := range map[string]string{"USD": "no_route", "BDT": "currency_not_accepted"} {
			r := f.do("POST", "/v1/payments", f.testKey, map[string]any{"amount": 100, "currency": cur})
			if r.status != 422 || r.errType(t) != want {
				t.Errorf("%s: %d %s", cur, r.status, r.raw)
			}
		}
		if e := f.do("POST", "/v1/payments", f.testKey, map[string]any{"amount": 100, "currency": "BDT"}).json(t)["error"].(map[string]any); e["message"] != "BDT is not accepted for this app" {
			t.Errorf("reject message not surfaced: %v", e)
		}
		r := f.do("POST", "/v1/payments", f.testKey, map[string]any{"amount": 100, "currency": "INR", "country": "IN", "allowed_connectors": []string{"razorpay"}})
		if r.status != 201 || seen.Country != "IN" || len(seen.AllowedConnectors) != 1 || seen.Scope.AppID != f.app.ID || seen.Scope.Mode != "test" {
			t.Errorf("%d %s %+v", r.status, r.raw, seen)
		}
		if f.count("SELECT COUNT(*) FROM payments") != 1 {
			t.Error("unroutable requests created payments")
		}
	})
}

// ---- rate limit ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestRateLimitIs20PerSecondPerKey(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		clk := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
		f := newFixtureOpts(t, db, nil, func(o *httpapi.Options) { o.RatePerSecond = 0; o.Now = clk.Now }) // 0 = the default, 20
		for i := 0; i < 20; i++ {
			if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 200 {
				t.Fatalf("request %d: %d %s", i+1, r.status, r.raw)
			}
		}
		r := f.do("GET", "/v1/payments", f.testKey, nil)
		if r.status != 429 || r.errType(t) != "rate_limited" {
			t.Fatalf("21st request: %d %s", r.status, r.raw)
		}
		if ra, err := strconv.Atoi(r.header.Get("Retry-After")); err != nil || ra < 1 {
			t.Errorf("Retry-After %q", r.header.Get("Retry-After"))
		}
		// Another key has its own bucket, and so does a different mode of the same app.
		if r := f.do("GET", "/v1/payments", f.otherKey, nil); r.status != 200 {
			t.Errorf("another key: %d", r.status)
		}
		if r := f.do("GET", "/v1/payments", f.liveKey, nil); r.status != 200 {
			t.Errorf("another key of the same app: %d", r.status)
		}
		// Tokens refill at 20 per second.
		clk.Add(49 * time.Millisecond)
		if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 429 {
			t.Errorf("before a full token: %d", r.status)
		}
		clk.Add(2 * time.Millisecond)
		if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 200 {
			t.Errorf("after 51ms: %d", r.status)
		}
		clk.Add(5 * time.Second) // never more than the burst
		ok := 0
		for i := 0; i < 30; i++ {
			if f.do("GET", "/v1/payments", f.testKey, nil).status == 200 {
				ok++
			}
		}
		if ok != 20 {
			t.Errorf("burst after a long idle: %d", ok)
		}
	})
}

func TestRateLimitedRequestsDoNotRunOrConsumeIdempotency(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		clk := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
		f := newFixtureOpts(t, db, nil, func(o *httpapi.Options) { o.RatePerSecond = 1; o.RateBurst = 1; o.Now = clk.Now })
		if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 200 {
			t.Fatal(r.status)
		}
		r := f.do("POST", "/v1/payments", f.testKey, createBody, "Idempotency-Key", "k-1")
		if r.status != 429 || f.count("SELECT COUNT(*) FROM payments") != 0 || f.count("SELECT COUNT(*) FROM idempotency_keys") != 0 {
			t.Fatalf("limited POST: %d, payments=%d keys=%d", r.status, f.count("SELECT COUNT(*) FROM payments"), f.count("SELECT COUNT(*) FROM idempotency_keys"))
		}
		clk.Add(time.Second)
		if r := f.do("POST", "/v1/payments", f.testKey, createBody, "Idempotency-Key", "k-1"); r.status != 201 {
			t.Errorf("retry after the wait: %d %s", r.status, r.raw)
		}
	})
}

func TestUnauthenticatedRequestsDoNotConsumeAKeysBudget(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		clk := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
		f := newFixtureOpts(t, db, nil, func(o *httpapi.Options) { o.RatePerSecond = 1; o.RateBurst = 1; o.Now = clk.Now })
		for i := 0; i < 5; i++ {
			if r := f.do("GET", "/v1/payments", "pmx_test_sk_"+strings.Repeat("a", 43), nil); r.status != 401 {
				t.Fatalf("%d", r.status)
			}
		}
		if r := f.do("GET", "/v1/payments", f.testKey, nil); r.status != 200 {
			t.Errorf("a valid key was limited by other people's bad requests: %d", r.status)
		}
	})
}
