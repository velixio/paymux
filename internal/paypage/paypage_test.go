package paypage_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/domains"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/paypage"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/web"
	"github.com/paymux/paymux/pkg/connector"
)

var (
	ctx = context.Background()
	org = core.OrgScope{OrgID: store.DefaultOrgID}
)

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
	t     *testing.T
	db    *store.DB
	core  *core.Service
	set   *settings.Service
	fake  *fakeconn.State
	clk   *clock
	srv   *httptest.Server
	scope core.Scope
	pa    string
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	clk := &clock{t: time.Now().UTC()}
	e := &env{t: t, db: db, clk: clk, set: settings.New(db), fake: fakeconn.Register("fake"), scope: core.Scope{AppID: "app_1", Mode: "test"}}
	q := jobs.New(db, jobs.Options{Clock: clk.Now})
	box, _ := secrets.NewBox(bytes.Repeat([]byte{5}, 32))
	base, _ := url.Parse("https://pay.example.com")
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now})
	br := routing.NewBreaker(clk.Now)
	rt := checkout.NewRouting(br)
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: q, Router: rt, Now: clk.Now})
	flow := checkout.New(checkout.Options{DB: db, Core: e.core, Conns: conns, Routing: rt, Breaker: br, Now: clk.Now})
	if _, err := core.Bootstrap(ctx, db, e.set); err != nil {
		t.Fatal(err)
	}
	err := db.Write(ctx, func(tx store.Tx) error {
		for _, id := range []string{"app_1", "app_2"} {
			if err := store.InsertApp(ctx, tx, store.App{ID: id, OrgID: store.DefaultOrgID, Name: "Shop " + id, Slug: id, CreatedAt: clk.t,
				DefaultReturnURL: ""}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pa, err := conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: "main", Mode: "test",
		Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
	if err != nil {
		t.Fatal(err)
	}
	e.pa = pa.ID
	if err := conns.LinkApp(ctx, audit.System, org, pa.ID, "app_2"); err != nil {
		t.Fatal(err)
	}
	doc := `{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + e.fake.Name + `"}]}}}`
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		t.Fatal(err)
	}
	rend, err := web.NewRenderer(web.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	h := paypage.New(paypage.Options{Core: e.core, Flow: flow, Settings: e.set, Renderer: rend, Assets: web.AssetHandler(web.Embedded()),
		CSRFKey: bytes.Repeat([]byte{9}, 32), Now: clk.Now})
	mux := http.NewServeMux()
	h.Register(paypage.StdMux(mux))
	router := &domains.Router{BaseHost: "pay.example.com", Lookup: e.core, Base: mux, Customer: mux}
	e.srv = httptest.NewServer(router)
	t.Cleanup(e.srv.Close)
	return e
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

type resp struct {
	status int
	header http.Header
	body   string
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *env) do(method, path, host string, form url.Values) resp {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	req.Host = "pay.example.com"
	if host != "" {
		req.Host = host
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, string(b)}
}

type created struct {
	id, secret string
}

func (c created) path() string { return "/pay/" + c.id + "?t=" + url.QueryEscape(c.secret) }

func (e *env) create(in core.CreatePaymentInput) created {
	e.t.Helper()
	in.Scope = e.scope
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.Amount == 0 {
		in.Amount = 1000
	}
	res, err := e.core.CreatePayment(ctx, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return created{res.Payment.ID, res.ClientSecret}
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) payment(id string) store.Payment {
	p, err := e.core.GetPaymentByID(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// ---- pay page ----

func TestPayRedirects(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{})
		r := e.do("GET", c.path(), "", nil)
		if r.status != 303 || !strings.HasPrefix(r.header.Get("Location"), "https://fake.example.com/checkout/") {
			t.Fatalf("%d %q %s", r.status, r.header.Get("Location"), r.body)
		}
		if r.header.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(r.header.Get("Cache-Control"), "no-store") {
			t.Errorf("pages carrying the secret in the URL need no-referrer and no-store: %v", r.header)
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id) != 1 {
			t.Error("attempt not created")
		}
	})
}

func TestPayReloadReusesTheAttemptAndFreshStartsANewOne(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{})
		first := e.do("GET", c.path(), "", nil)
		again := e.do("GET", c.path(), "", nil)
		if first.status != 303 || again.status != 303 || first.header.Get("Location") != again.header.Get("Location") {
			t.Fatalf("a reload changed the checkout: %q then %q", first.header.Get("Location"), again.header.Get("Location"))
		}
		if n := e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id); n != 1 {
			t.Errorf("%d attempts after a reload, want 1", n)
		}
		// "Pay another way" or "try again" asks for a new attempt explicitly.
		fresh := e.do("GET", c.path()+"&fresh=1", "", nil)
		if fresh.status != 303 || fresh.header.Get("Location") == first.header.Get("Location") {
			t.Errorf("fresh=1 did not start a new checkout: %d %q", fresh.status, fresh.header.Get("Location"))
		}
		if n := e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id); n != 2 {
			t.Errorf("%d attempts after fresh=1, want 2", n)
		}
	})
}

func TestPayRejectsWrongSecret(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{})
		other := e.create(core.CreatePaymentInput{})
		for name, path := range map[string]string{
			"no secret": "/pay/" + c.id, "wrong secret": "/pay/" + c.id + "?t=nope", "other payments secret": "/pay/" + c.id + "?t=" + other.secret,
			"unknown payment": "/pay/pay_missing?t=" + c.secret,
		} {
			r := e.do("GET", path, "", nil)
			if r.status != 404 || strings.Contains(r.body, c.id) {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts") != 0 {
			t.Error("an attempt was created for an unauthenticated request")
		}
	})
}

func TestPayFormPostAndSDK(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{ProviderOrderID: "o", NextAction: connector.NextAction{Kind: connector.ActionFormPost,
					URL: "https://secure.payu.example.com/_payment", Fields: map[string]string{"txnid": "T1", "hash": "abc\"def", "amount": "10.00"}}}, nil
			}
		})
		r := e.do("GET", e.create(core.CreatePaymentInput{}).path(), "", nil)
		if r.status != 200 || !strings.Contains(r.body, `action="https://secure.payu.example.com/_payment"`) ||
			!strings.Contains(r.body, `name="txnid" value="T1"`) || !strings.Contains(r.body, `name="hash" value="abc&#34;def"`) ||
			!strings.Contains(r.body, "Shop app_1") {
			t.Fatalf("form_post: %d %s", r.status, r.body)
		}
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{ProviderOrderID: "o", NextAction: connector.NextAction{Kind: connector.ActionSDK,
					ScriptURL: "https://checkout.example.com/v1/checkout.js", Options: map[string]any{"key": "rzp_test", "order_id": "order_1", "amount": 1000}}}, nil
			}
		})
		r = e.do("GET", e.create(core.CreatePaymentInput{}).path(), "", nil)
		if r.status != 200 || !strings.Contains(r.body, `src="https://checkout.example.com/v1/checkout.js"`) ||
			!strings.Contains(r.body, "order_1") || !strings.Contains(r.body, `"`+e.fake.Name+`"`) {
			t.Fatalf("sdk: %d %s", r.status, r.body)
		}
	})
}

func TestPayFinishedPaymentsRenderResultPages(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		canceled := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/back"})
		if _, err := e.core.CancelPayment(ctx, e.scope, audit.System, canceled.id); err != nil {
			t.Fatal(err)
		}
		if r := e.do("GET", canceled.path(), "", nil); r.status != 200 || !strings.Contains(strings.ToLower(r.body), "canceled") {
			t.Errorf("canceled: %d %s", r.status, r.body)
		}

		expiring := e.create(core.CreatePaymentInput{})
		e.clk.Add(2 * time.Hour) // default ttl is 60 minutes
		r := e.do("GET", expiring.path(), "", nil)
		if r.status != 200 || !strings.Contains(strings.ToLower(r.body), "expired") || r.header.Get("Location") != "" {
			t.Errorf("expired: %d %s", r.status, r.body)
		}

		e.clk.Add(-2 * time.Hour)
		paid := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/back"})
		att, _ := e.core.CreateAttempt(ctx, paid.id, e.pa)
		if _, err := e.core.ApplyAttemptSnapshot(ctx, att.ID, core.Snapshot{Status: "captured", Amount: 1000, Currency: "USD"}); err != nil {
			t.Fatal(err)
		}
		r = e.do("GET", paid.path(), "", nil)
		if r.status != 200 || !strings.Contains(r.body, "https://shop.example.com/back?payment_id="+paid.id+"&amp;status=succeeded") {
			t.Errorf("succeeded: %d %s", r.status, r.body)
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id IN (?, ?)", canceled.id, expiring.id) != 0 {
			t.Error("finished payments must not start attempts")
		}
	})
}

func TestPayShowsFailureWhenNoProviderWorks(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{}, connector.ErrTemporary
			}
		})
		c := e.create(core.CreatePaymentInput{})
		r := e.do("GET", c.path(), "", nil)
		if r.status != http.StatusBadGateway || !strings.Contains(r.body, "try again") || strings.Contains(r.body, "temporary") {
			t.Errorf("%d %s", r.status, r.body)
		}
	})
}

// ---- billing ----

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (e *env) inrPayment(amount int64) created {
	return e.create(core.CreatePaymentInput{Currency: "INR", Amount: amount, ReturnURL: "https://shop.example.com/back"})
}

func TestBillingFormShownForIndianB2CWhenInvoicing(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.inrPayment(50000)
		r := e.do("GET", c.path(), "", nil)
		if r.status != 200 || !strings.Contains(r.body, `name="state_code"`) || !strings.Contains(r.body, "Karnataka") || csrfRE.FindStringSubmatch(r.body) == nil {
			t.Fatalf("billing form: %d %s", r.status, r.body)
		}
		if strings.Contains(r.body, `name="line1"`) {
			t.Error("full address is only needed from 50,000 rupees")
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts") != 0 {
			t.Error("checkout started before billing details")
		}
		big := e.inrPayment(5_000_000)
		if r := e.do("GET", big.path(), "", nil); !strings.Contains(r.body, `name="line1"`) || !strings.Contains(r.body, `name="postal_code"`) {
			t.Errorf("address fields missing at 50,000: %s", r.body)
		}
		// Not needed for foreign currency, nor with invoicing off, nor when the setting is off.
		if r := e.do("GET", e.create(core.CreatePaymentInput{Currency: "USD"}).path(), "", nil); r.status != 303 {
			t.Errorf("USD: %d", r.status)
		}
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "issue_invoices", json.RawMessage(`false`)); err != nil {
			t.Fatal(err)
		}
		if r := e.do("GET", e.inrPayment(50000).path(), "", nil); r.status != 303 {
			t.Errorf("invoicing off: %d %s", r.status, r.body)
		}
		_ = e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "issue_invoices", json.RawMessage(`true`))
		_ = e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "billing_details", json.RawMessage(`"off"`))
		if r := e.do("GET", e.inrPayment(50000).path(), "", nil); r.status != 303 {
			t.Errorf("billing_details off: %d", r.status)
		}
	})
}

func TestBillingFormSkippedWhenCustomerAlreadyHasState(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{Currency: "INR", Amount: 50000,
			Customer: &core.CustomerInput{Name: "Asha", Billing: store.Billing{StateCode: "29"}}})
		if r := e.do("GET", c.path(), "", nil); r.status != 303 {
			t.Errorf("known state should skip the form: %d %s", r.status, r.body)
		}
	})
}

func (e *env) billingPost(c created, mod func(url.Values)) resp {
	e.t.Helper()
	form := url.Values{"name": {"Asha K"}, "email": {"asha@example.com"}, "state_code": {"29"}}
	page := e.do("GET", c.path(), "", nil)
	m := csrfRE.FindStringSubmatch(page.body)
	if m == nil {
		e.t.Fatalf("no csrf token in %d %s", page.status, page.body)
	}
	form.Set("csrf", m[1])
	if mod != nil {
		mod(form)
	}
	return e.do("POST", c.path(), "", form)
}

func TestBillingPostStoresDetailsThenContinues(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.inrPayment(50000)
		r := e.billingPost(c, func(f url.Values) {
			f.Set("gstin", "29AAGCB7383J1Z4")
			f.Set("legal_name", "Bright Labs Pvt Ltd")
		})
		if r.status != 303 || r.header.Get("Location") != c.path() {
			t.Fatalf("%d %q %s", r.status, r.header.Get("Location"), r.body)
		}
		p := e.payment(c.id)
		cust, err := e.core.GetCustomer(ctx, p.CustomerID)
		if err != nil || cust.Name != "Asha K" || cust.Email != "asha@example.com" || cust.Billing.StateCode != "29" ||
			cust.Billing.GSTIN != "29AAGCB7383J1Z4" || cust.Billing.LegalName != "Bright Labs Pvt Ltd" {
			t.Fatalf("customer %+v %v", cust, err)
		}
		if r := e.do("GET", c.path(), "", nil); r.status != 303 || !strings.HasPrefix(r.header.Get("Location"), "https://fake.example.com/") {
			t.Errorf("after billing the checkout starts: %d %q", r.status, r.header.Get("Location"))
		}
	})
}

func TestBillingPostValidation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.inrPayment(50000)
		cases := map[string]func(url.Values){
			"missing state":     func(f url.Values) { f.Del("state_code") },
			"unknown state":     func(f url.Values) { f.Set("state_code", "99") },
			"missing name":      func(f url.Values) { f.Set("name", " ") },
			"bad email":         func(f url.Values) { f.Set("email", "nope") },
			"bad gstin":         func(f url.Values) { f.Set("gstin", "29AAGCB7383J1Z5") },
			"gstin no legal":    func(f url.Values) { f.Set("gstin", "29AAGCB7383J1Z4") },
			"gstin other state": func(f url.Values) { f.Set("gstin", "27AAPFU0939F1ZV"); f.Set("legal_name", "X") },
			"bad csrf":          func(f url.Values) { f.Set("csrf", "00") },
			"missing csrf":      func(f url.Values) { f.Del("csrf") },
		}
		for name, mod := range cases {
			r := e.billingPost(c, mod)
			want := 400
			if strings.Contains(name, "csrf") {
				want = 403
			}
			if r.status != want {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
			if want == 400 && !strings.Contains(r.body, `class="error"`) {
				t.Errorf("%s: no error shown: %s", name, r.body)
			}
		}
		if e.payment(c.id).CustomerID != "" {
			t.Error("invalid submissions stored a customer")
		}
		// A CSRF token of another payment is not accepted.
		other := e.inrPayment(50000)
		page := e.do("GET", other.path(), "", nil)
		tok := csrfRE.FindStringSubmatch(page.body)[1]
		r := e.do("POST", c.path(), "", url.Values{"csrf": {tok}, "name": {"A"}, "state_code": {"29"}})
		if r.status != 403 {
			t.Errorf("csrf token reuse across payments: %d", r.status)
		}
		// Wrong payment secret on POST.
		r = e.do("POST", "/pay/"+c.id+"?t=bad", "", url.Values{"csrf": {tok}})
		if r.status != 404 {
			t.Errorf("post with wrong secret: %d", r.status)
		}
	})
}

func TestBillingPostRequiresAddressAtHighAmounts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.inrPayment(5_000_000)
		if r := e.billingPost(c, nil); r.status != 400 {
			t.Errorf("missing address accepted: %d", r.status)
		}
		if r := e.billingPost(c, func(f url.Values) { f.Set("line1", "1 MG Road"); f.Set("postal_code", "56001") }); r.status != 400 {
			t.Errorf("bad PIN accepted: %d", r.status)
		}
		r := e.billingPost(c, func(f url.Values) { f.Set("line1", "1 MG Road"); f.Set("postal_code", "560001") })
		if r.status != 303 {
			t.Fatalf("%d %s", r.status, r.body)
		}
		cust, _ := e.core.GetCustomer(ctx, e.payment(c.id).CustomerID)
		if cust.Billing.Line1 != "1 MG Road" || cust.Billing.PostalCode != "560001" || cust.Billing.Country != "IN" {
			t.Errorf("%+v", cust.Billing)
		}
	})
}

// ---- return ----

func (e *env) startAndAttempt(c created) (store.Attempt, string) {
	e.t.Helper()
	r := e.do("GET", c.path(), "", nil)
	if r.status != 303 {
		e.t.Fatalf("start: %d %s", r.status, r.body)
	}
	var a store.Attempt
	_ = e.db.Read(ctx, func(q store.Querier) error {
		l, err := store.ListAttempts(ctx, q, c.id)
		if err == nil && len(l) > 0 {
			a = l[len(l)-1]
		}
		return err
	})
	return a, "/return/" + e.pa + "/" + a.ProviderRef
}

func TestReturnAppliesTruthAndRedirectsToApp(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/back?x=1"})
		a, path := e.startAndAttempt(c)
		e.fake.Locked(func() {
			e.fake.Payments[a.ProviderRef] = connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 1000, Currency: "USD", ProviderPaymentID: "pay_1"}
		})
		for _, method := range []string{"GET", "POST"} {
			var form url.Values
			target := path + "?sig=ok&payment_id=pay_1"
			if method == "POST" {
				form, target = url.Values{"sig": {"ok"}, "payment_id": {"pay_1"}}, path
			}
			r := e.do(method, target, "", form)
			loc, _ := url.Parse(r.header.Get("Location"))
			if r.status != 303 || loc == nil || loc.Host != "shop.example.com" || loc.Query().Get("payment_id") != c.id ||
				loc.Query().Get("status") != "succeeded" || loc.Query().Get("x") != "1" {
				t.Fatalf("%s: %d %q %s", method, r.status, r.header.Get("Location"), r.body)
			}
		}
		if e.payment(c.id).Status != "succeeded" {
			t.Error("payment not applied")
		}
	})
}

func TestReturnWithoutReturnURLRendersResultPage(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{})
		a, path := e.startAndAttempt(c)
		e.fake.Locked(func() {
			e.fake.Payments[a.ProviderRef] = connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: 1000, Currency: "USD", ProviderPaymentID: "pay_1"}
		})
		r := e.do("GET", path+"?sig=ok&payment_id=pay_1", "", nil)
		if r.status != 200 || !strings.Contains(strings.ToLower(r.body), "payment received") {
			t.Errorf("%d %s", r.status, r.body)
		}
	})
}

func TestReturnForgedOrUnknown(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/back"})
		_, path := e.startAndAttempt(c)
		r := e.do("GET", path+"?status=success&payment_id=pay_forged", "", nil)
		loc, _ := url.Parse(r.header.Get("Location"))
		if r.status != 303 || loc.Query().Get("status") == "succeeded" || e.payment(c.id).Status == "succeeded" {
			t.Errorf("forged return: %d %q", r.status, r.header.Get("Location"))
		}
		if r := e.do("GET", "/return/"+e.pa+"/NOPE", "", nil); r.status != 404 {
			t.Errorf("unknown ref: %d", r.status)
		}
		if r := e.do("GET", "/return/pa_other/"+strings.TrimPrefix(path, "/return/"+e.pa+"/"), "", nil); r.status != 404 {
			t.Errorf("wrong account: %d", r.status)
		}
	})
}

// ---- domains and assets ----

func TestCustomDomainServesOnlyItsOwnApp(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if _, err := e.core.AddDomain(ctx, audit.System, org, "app_1", "pay.shop.example.org"); err != nil {
			t.Fatal(err)
		}
		mine := e.create(core.CreatePaymentInput{})
		if r := e.do("GET", mine.path(), "pay.shop.example.org", nil); r.status != 303 {
			t.Errorf("own payment on own domain: %d %s", r.status, r.body)
		}
		e.scope = core.Scope{AppID: "app_2", Mode: "test"}
		theirs := e.create(core.CreatePaymentInput{})
		if r := e.do("GET", theirs.path(), "pay.shop.example.org", nil); r.status != 404 {
			t.Errorf("another app's payment on this domain: %d %s", r.status, r.body)
		}
		if r := e.do("GET", theirs.path(), "", nil); r.status != 303 {
			t.Errorf("base host serves every app: %d", r.status)
		}
		if r := e.do("GET", mine.path(), "unknown.example.org", nil); r.status != http.StatusMisdirectedRequest {
			t.Errorf("unknown host: %d", r.status)
		}
	})
}

func TestAssetsServed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		r := e.do("GET", "/assets/css/app.css", "", nil)
		if r.status != 200 || !strings.Contains(r.body, "--color-accent") {
			t.Errorf("%d %.80s", r.status, r.body)
		}
	})
}

func TestWellKnownServesTheActivationToken(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d, err := e.core.AddDomain(ctx, audit.System, org, "app_1", "pay.shop.example.org")
		if err != nil {
			t.Fatal(err)
		}
		r := e.do("GET", "/.well-known/paymux/"+d.Token, "pay.shop.example.org", nil)
		if r.status != 200 || strings.TrimSpace(r.body) != d.Token || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") {
			t.Errorf("token file: %d %q %v", r.status, r.body, r.header)
		}
		if r := e.do("GET", "/.well-known/paymux/wrong", "pay.shop.example.org", nil); r.status != 404 {
			t.Errorf("wrong token: %d", r.status)
		}
		if r := e.do("GET", "/.well-known/paymux/"+d.Token, "", nil); r.status != 404 {
			t.Errorf("the base host has no activation file: %d", r.status)
		}
		if r := e.do("GET", "/.well-known/paymux/"+d.Token, "other.example.org", nil); r.status != http.StatusMisdirectedRequest {
			t.Errorf("unknown host: %d", r.status)
		}
	})
}
