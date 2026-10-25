package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// roleClient seeds a user with the role and returns a logged-in client.
func (e *env) roleClient(role string) *http.Client {
	e.t.Helper()
	email := role + "@example.com"
	if _, err := e.db.UserByEmail(ctx, email); err == nil {
		e.t.Fatalf("%s already logged in", role)
	}
	_, secret := e.seedUser(email, role, true)
	c := e.client()
	e.login(c, email, secret)
	return c
}

func (e *env) seedApp(slug string) store.App {
	e.t.Helper()
	app, err := e.core.CreateApp(ctx, audit.System, core.OrgScope{OrgID: "org_default"}, core.CreateAppInput{Name: strings.ToUpper(slug[:1]) + slug[1:], Slug: slug})
	if err != nil {
		e.t.Fatal(err)
	}
	return app
}

func (e *env) seedPayment(id, appID, mode, status, orderRef string, amount int64, created time.Time) store.Payment {
	e.t.Helper()
	p := store.Payment{ID: id, OrgID: store.DefaultOrgID, AppID: appID, Mode: mode, Kind: "one_time", Amount: amount, Currency: "INR",
		Status: status, OrderRef: orderRef, Metadata: map[string]string{}, ClientSecretHash: "h", ExpiresAt: created.Add(time.Hour),
		Version: 1, Flags: []string{}, CreatedAt: created, UpdatedAt: created}
	if status == "succeeded" {
		p.AmountReceived = amount
	}
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertPayment(ctx, tx, p) }); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) seedAttempt(a store.Attempt) {
	e.t.Helper()
	a.OrgID, a.CreatedAt, a.UpdatedAt = store.DefaultOrgID, e.clk.Now(), e.clk.Now()
	if err := e.db.Write(ctx, func(tx store.Tx) error { return store.InsertAttempt(ctx, tx, a) }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) pageOK(c *http.Client, path string) string {
	e.t.Helper()
	r, body := e.get(c, path)
	if r.StatusCode != 200 {
		e.t.Fatalf("GET %s = %d\n%s", path, r.StatusCode, body)
	}
	return body
}

func (e *env) setMode(c *http.Client, mode string) {
	e.t.Helper()
	tok := csrfOf(e.t, e.pageOK(c, "/dashboard/payments"))
	r, _ := e.post(c, "/dashboard/mode", url.Values{"mode": {mode}, "next": {"/dashboard/payments"}, "csrf": {tok}})
	if r.StatusCode != 303 {
		e.t.Fatalf("set mode: %d", r.StatusCode)
	}
}

func has(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func lacks(t *testing.T, body string, nope ...string) {
	t.Helper()
	for _, w := range nope {
		if strings.Contains(body, w) {
			t.Errorf("page unexpectedly has %q", w)
		}
	}
}

func TestPaymentListFiltersModeAndPaging(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		a, b := e.seedApp("alpha"), e.seedApp("beta")
		day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		e.seedPayment("pay_a1", a.ID, "test", "succeeded", "ORD-100", 1000, day)
		e.seedPayment("pay_a2", a.ID, "test", "failed", "ORD-200", 2000, day.Add(24*time.Hour))
		e.seedPayment("pay_a3", a.ID, "test", "succeeded", "ORD-300", 3000, day.Add(48*time.Hour))
		e.seedPayment("pay_b1", b.ID, "test", "succeeded", "ORD-400", 4000, day.Add(72*time.Hour))
		e.seedPayment("pay_l1", a.ID, "live", "succeeded", "ORD-LIVE", 5000, day)
		c := e.roleClient("viewer")

		// default mode is live
		body := e.pageOK(c, "/dashboard/payments")
		has(t, body, "href=\"/dashboard/payments/pay_l1\"", "ORD-LIVE")
		lacks(t, body, "href=\"/dashboard/payments/pay_a1\"", "href=\"/dashboard/payments/pay_b1\"")
		e.setMode(c, "test")
		body = e.pageOK(c, "/dashboard/payments")
		has(t, body, "href=\"/dashboard/payments/pay_b1\"", "href=\"/dashboard/payments/pay_a3\"") // newest two (page size 2)
		lacks(t, body, "href=\"/dashboard/payments/pay_a2\"", "href=\"/dashboard/payments/pay_l1\"")

		// cursor pagination keeps the filters
		m := regexpFind(t, `href="([^"]*after=pay_a3[^"]*)"`, body)
		m = strings.ReplaceAll(m, "&amp;", "&")
		body = e.pageOK(c, m)
		has(t, body, "href=\"/dashboard/payments/pay_a2\"", "href=\"/dashboard/payments/pay_a1\"")
		lacks(t, body, "href=\"/dashboard/payments/pay_b1\"", "href=\"/dashboard/payments/pay_a3\"", `class="next"`)

		// filters
		for name, c2 := range map[string]struct {
			query string
			want  []string
			not   []string
		}{
			"app":       {"?app=" + b.ID, []string{"href=\"/dashboard/payments/pay_b1\""}, []string{"href=\"/dashboard/payments/pay_a1\"", "href=\"/dashboard/payments/pay_a3\""}},
			"status":    {"?status=failed", []string{"href=\"/dashboard/payments/pay_a2\""}, []string{"href=\"/dashboard/payments/pay_a1\"", "href=\"/dashboard/payments/pay_b1\""}},
			"order ref": {"?q=ord-100", []string{"href=\"/dashboard/payments/pay_a1\""}, []string{"href=\"/dashboard/payments/pay_a2\""}},
			"id":        {"?q=pay_b1", []string{"href=\"/dashboard/payments/pay_b1\""}, []string{"href=\"/dashboard/payments/pay_a3\""}},
			"dates":     {"?from=2026-10-02&to=2026-10-03", []string{"href=\"/dashboard/payments/pay_a2\"", "href=\"/dashboard/payments/pay_a3\""}, []string{"href=\"/dashboard/payments/pay_a1\"", "href=\"/dashboard/payments/pay_b1\""}},
		} {
			body := e.pageOK(c, "/dashboard/payments"+c2.query)
			t.Log("filter case", name)
			has(t, body, c2.want...)
			lacks(t, body, c2.not...)
		}
		if r, _ := e.get(c, "/dashboard/payments?status=bogus"); r.StatusCode != 400 {
			t.Errorf("bad status: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/payments?from=yesterday"); r.StatusCode != 400 {
			t.Errorf("bad date: %d", r.StatusCode)
		}
	})
}

func regexpFind(t *testing.T, pattern, s string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s in page", pattern)
	}
	return m[1]
}

func TestModeToggleIsPerSessionAndUnforgeable(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		a := e.seedApp("alpha")
		e.seedPayment("pay_t", a.ID, "test", "succeeded", "T", 1, e.clk.Now())
		e.seedPayment("pay_l", a.ID, "live", "succeeded", "L", 1, e.clk.Now())
		c := e.roleClient("viewer")
		e.setMode(c, "test")
		body := e.pageOK(c, "/dashboard/payments")
		has(t, body, "pay_t")
		lacks(t, body, "pay_l")
		has(t, body, `name="mode" value="live"`) // offers the other mode

		// a forged cookie falls back to live
		c2 := e.roleClient("operator")
		u, _ := url.Parse(e.srv.URL)
		c2.Jar.SetCookies(u, []*http.Cookie{{Name: "pm_mode", Value: "test.forged", Path: "/dashboard"}})
		body = e.pageOK(c2, "/dashboard/payments")
		has(t, body, "pay_l")
		lacks(t, body, "pay_t")
		// so does the genuine cookie of another session
		for _, ck := range c.Jar.Cookies(u) {
			if ck.Name == "pm_mode" {
				c2.Jar.SetCookies(u, []*http.Cookie{{Name: "pm_mode", Value: ck.Value, Path: "/dashboard"}})
			}
		}
		body = e.pageOK(c2, "/dashboard/payments")
		lacks(t, body, "pay_t")

		// validation, CSRF and open-redirect protection
		tok := csrfOf(t, body)
		if r, _ := e.post(c2, "/dashboard/mode", url.Values{"mode": {"staging"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("bad mode: %d", r.StatusCode)
		}
		if r, _ := e.post(c2, "/dashboard/mode", url.Values{"mode": {"test"}}); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
		r, _ := e.post(c2, "/dashboard/mode", url.Values{"mode": {"test"}, "next": {"https://evil.example/x"}, "csrf": {tok}})
		if r.StatusCode != 303 || strings.Contains(r.Header.Get("Location"), "evil") {
			t.Errorf("open redirect: %s", r.Header.Get("Location"))
		}
	})
}

// detailFixture builds a payment with attempts, ledger, a refund, an event and
// a delivered webhook.
type detailFixture struct {
	app  store.App
	pay  store.Payment
	sink *sink
}

type sink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
}

func (e *env) detailFixture() detailFixture {
	e.t.Helper()
	app := e.seedApp("alpha")
	p := e.seedPayment("pay_d1", app.ID, "live", "succeeded", "ORD-D1", 10000, e.clk.Now())
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccountInfo(ctx, tx, store.ProviderAccountInfo{ID: "pa_rzp", OrgID: store.DefaultOrgID, Mode: "live",
			Connector: "razorpay", Name: "Razorpay main", Status: "active", Currencies: []string{"INR"}, Purposes: []string{"one_time"}}, e.clk.Now())
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.seedAttempt(store.Attempt{ID: "att_fail", Mode: "live", PaymentID: p.ID, ProviderAccountID: "pa_rzp", ProviderRef: "pr_fail", ProviderOrderID: "order_F1",
		Status: "failed", Method: "card", FailureCode: "card_declined", FailureMessage: "Your card was declined"})
	e.seedAttempt(store.Attempt{ID: "att_ok", Mode: "live", PaymentID: p.ID, ProviderAccountID: "pa_rzp", ProviderRef: "pr_ok", ProviderOrderID: "order_OK",
		ProviderPaymentID: "pay_RZP_OK", Status: "captured", Method: "upi", AmountReceived: 10000, CurrencyReceived: "INR"})
	err = e.db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: "led_1", OrgID: store.DefaultOrgID, Mode: "live", Kind: "charge",
			Amount: 10000, Currency: "INR", PaymentID: p.ID, OccurredAt: e.clk.Now()}); err != nil {
			return err
		}
		return store.InsertRefund(ctx, tx, store.Refund{ID: "ref_old", OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live", PaymentID: p.ID, AttemptID: "att_ok",
			Amount: 1500, Currency: "INR", Status: "succeeded", Reason: "goodwill", ProviderRef: "pr_ref_old", InitiatedBy: "api", CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()})
	})
	if err != nil {
		e.t.Fatal(err)
	}

	s := &sink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		w.WriteHeader(200)
	}))
	e.t.Cleanup(s.srv.Close)
	if _, _, err := e.out.CreateEndpoint(ctx, outbound.CreateEndpointInput{AppID: app.ID, URL: s.srv.URL}); err != nil {
		e.t.Fatal(err)
	}
	err = e.db.Write(ctx, func(tx store.Tx) error {
		return e.out.Emit(ctx, tx, events.Event{OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live", Type: "payment.succeeded",
			ObjectType: "payment", ObjectID: p.ID, ObjectVersion: 2, Data: map[string]any{"id": p.ID}})
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.runJobs()
	return detailFixture{app: app, pay: p, sink: s}
}

func (e *env) runJobs() {
	e.t.Helper()
	for i := 0; i < 20; i++ {
		n, err := e.q.RunOnce(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (s *sink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.hits }

func TestPaymentDetailShowsTimeline(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		c := e.roleClient("viewer")
		body := e.pageOK(c, "/dashboard/payments/"+f.pay.ID)
		has(t, body, "pay_d1", "ORD-D1", "100.00 INR", "succeeded",
			// attempts
			"att_fail", "failed", "card_declined", "Your card was declined", "order_F1",
			"att_ok", "captured", "pay_RZP_OK", "order_OK", "upi", "Razorpay main", "razorpay",
			// ledger
			"charge", "100.00 INR",
			// refunds
			"ref_old", "15.00 INR", "goodwill", "api",
			// events and deliveries
			"payment.succeeded", f.sink.srv.URL, "delivered")
		// viewers get neither the refund form nor replay buttons
		lacks(t, body, `action="/dashboard/payments/pay_d1/refund"`, "/replay")

		if r, _ := e.get(c, "/dashboard/payments/pay_nope"); r.StatusCode != 404 {
			t.Errorf("missing payment: %d", r.StatusCode)
		}
		op := e.roleClient("operator")
		body = e.pageOK(op, "/dashboard/payments/"+f.pay.ID)
		has(t, body, `action="/dashboard/payments/pay_d1/refund"`, "/replay", "att_ok")
		// only captured, non-duplicate attempts can be picked for a refund
		opt := regexpFind(t, `(?s)(<select name="attempt_id".*?</select>)`, body)
		has(t, opt, "att_ok")
		lacks(t, opt, "att_fail")
	})
}

func TestRefundFromDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		path := "/dashboard/payments/" + f.pay.ID + "/refund"

		viewer := e.roleClient("viewer")
		tok := csrfOf(t, e.pageOK(viewer, "/dashboard/payments/"+f.pay.ID))
		if r, _ := e.post(viewer, path, url.Values{"amount": {"1.00"}, "csrf": {tok}}); r.StatusCode != 403 {
			t.Fatalf("viewer refund: %d", r.StatusCode)
		}

		op := e.roleClient("operator")
		tok = csrfOf(t, e.pageOK(op, "/dashboard/payments/"+f.pay.ID))
		// every rendering of the form carries its own single-use nonce
		nonce := func() string { return nonceOf(t, e.pageOK(op, "/dashboard/payments/"+f.pay.ID)) }
		// partial refund of an explicit attempt
		r, body := e.post(op, path, url.Values{"amount": {"25.50"}, "reason": {"customer asked"}, "attempt_id": {"att_ok"}, "csrf": {tok}, "nonce": {nonce()}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/payments/pay_d1" {
			t.Fatalf("refund: %d %s\n%s", r.StatusCode, r.Header.Get("Location"), body)
		}
		var refunds []store.Refund
		_ = db.Read(ctx, func(q store.Querier) (err error) { refunds, err = store.ListRefunds(ctx, q, f.pay.ID); return })
		var mine store.Refund
		for _, rf := range refunds {
			if rf.ID != "ref_old" {
				mine = rf
			}
		}
		opID := "usr_operator"
		if mine.ID == "" || mine.Amount != 2550 || mine.InitiatedBy != "dashboard" || mine.Actor != opID || mine.Reason != "customer asked" ||
			mine.Status != "pending" || mine.AttemptID != "att_ok" {
			t.Fatalf("refund row %+v", mine)
		}
		// audited with the user as actor
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "refund.create", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorType != "user" || aud[0].ActorID != opID || aud[0].Target != mine.ID {
			t.Fatalf("audit %+v", aud)
		}

		// blank amount refunds everything still refundable: 100.00 - 15.00 - 25.50 = 59.50
		r, _ = e.post(op, path, url.Values{"amount": {""}, "csrf": {tok}, "nonce": {nonce()}})
		if r.StatusCode != 303 {
			t.Fatalf("full refund: %d", r.StatusCode)
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) { refunds, err = store.ListRefunds(ctx, q, f.pay.ID); return })
		var sum int64
		for _, rf := range refunds {
			sum += rf.Amount
		}
		if sum != 10000 {
			t.Fatalf("refunded total %d, want 10000", sum)
		}
		// nothing left
		r, body = e.post(op, path, url.Values{"amount": {"1.00"}, "csrf": {tok}, "nonce": {nonce()}})
		if r.StatusCode != 400 || !strings.Contains(body, "exceeds the refundable balance") {
			t.Fatalf("over-refund: %d", r.StatusCode)
		}
		for name, form := range map[string]url.Values{
			"not a number": {"amount": {"abc"}},
			"negative":     {"amount": {"-5"}},
			"too precise":  {"amount": {"1.005"}},
			"bad attempt":  {"amount": {"1.00"}, "attempt_id": {"att_nope"}},
		} {
			form.Set("csrf", tok)
			form.Set("nonce", nonce())
			if r, _ := e.post(op, path, form); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if r, _ := e.post(op, "/dashboard/payments/pay_nope/refund", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown payment: %d", r.StatusCode)
		}
		if r, _ := e.post(op, path, url.Values{"amount": {"1.00"}}); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
	})
}

func TestReplayDeliveryFromDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		if f.sink.count() != 1 {
			t.Fatalf("setup hits=%d", f.sink.count())
		}
		var delID string
		_ = db.Read(ctx, func(q store.Querier) error {
			rows, err := store.ListPaymentDeliveries(ctx, q, f.pay.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("deliveries %v %v", rows, err)
			}
			delID = rows[0].DeliveryID
			return nil
		})
		viewer := e.roleClient("viewer")
		vt := csrfOf(t, e.pageOK(viewer, "/dashboard/payments/"+f.pay.ID))
		if r, _ := e.post(viewer, "/dashboard/deliveries/"+delID+"/replay", url.Values{"csrf": {vt}}); r.StatusCode != 403 {
			t.Fatalf("viewer replay: %d", r.StatusCode)
		}

		op := e.roleClient("operator")
		body := e.pageOK(op, "/dashboard/payments/"+f.pay.ID)
		tok := csrfOf(t, body)
		has(t, body, `action="/dashboard/deliveries/`+delID+`/replay"`)
		r, _ := e.post(op, "/dashboard/deliveries/"+delID+"/replay", url.Values{"csrf": {tok}, "next": {"/dashboard/payments/" + f.pay.ID}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/payments/"+f.pay.ID {
			t.Fatalf("replay: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		e.runJobs()
		if f.sink.count() != 2 {
			t.Fatalf("hits after replay = %d", f.sink.count())
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "webhook.replay", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" || aud[0].Target != delID {
			t.Fatalf("audit %+v", aud)
		}
		if r, _ := e.post(op, "/dashboard/deliveries/whd_nope/replay", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown delivery: %d", r.StatusCode)
		}
	})
}
