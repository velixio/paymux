package paylinks_test

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/domains"
	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/paylinks"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/web"
)

type hostedEnv struct {
	*env
	srv *httptest.Server
}

func newHosted(t *testing.T, db *store.DB) *hostedEnv {
	t.Helper()
	e := newEnv(t, db)
	rend, err := web.NewThemedRenderer(web.ThemedOptions{Base: web.Embedded(), Themes: web.NewThemes(web.Bundled(), ""), Recheck: -1})
	if err != nil {
		t.Fatal(err)
	}
	h := paylinks.NewHosted(paylinks.HostedOptions{Service: e.links, Core: e.core, Renderer: rend, Settings: e.set, CSRFKey: []byte("0123456789abcdef0123456789abcdef"),
		Now: e.clock})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /l/{slug}", h.Show)
	mux.HandleFunc("POST /l/{slug}", h.Submit)
	router := &domains.Router{BaseHost: "pay.example.com", Lookup: e.core, Base: mux, Customer: mux}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &hostedEnv{env: e, srv: srv}
}

type page struct {
	status int
	header http.Header
	body   string
}

var noRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (h *hostedEnv) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: noRedirect}
}

func (h *hostedEnv) do(c *http.Client, method, path, host string, form url.Values) page {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	req.Host = "pay.example.com"
	if host != "" {
		req.Host = host
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return page{res.StatusCode, res.Header, string(b)}
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// submit loads the form in c and posts it with the given fields.
func (h *hostedEnv) submit(c *http.Client, slug, query string, fields url.Values) page {
	h.t.Helper()
	form := h.do(c, "GET", "/l/"+slug+query, "", nil)
	m := csrfRE.FindStringSubmatch(form.body)
	if m == nil {
		h.t.Fatalf("no form on the page (%d):\n%s", form.status, form.body)
	}
	f := url.Values{"csrf": {m[1]}}
	for k, v := range fields {
		f[k] = v
	}
	return h.do(c, "POST", "/l/"+slug+query, "", f)
}

var asha = url.Values{"name": {"Asha K"}, "email": {"asha@example.com"}, "phone": {"+91 98765 43210"}}

func hindiStrings(t *testing.T) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(web.Embedded(), "i18n/hi.json")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLinkPageShowsPriceAndCollectsWhoIsPaying(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(nil)
		r := h.do(h.browser(), "GET", "/l/"+l.Slug, "", nil)
		if r.status != 200 {
			t.Fatalf("%d %s", r.status, r.body)
		}
		for _, want := range []string{"Voice Studio", "Course fee", "INR 499.00", `name="name"`, `name="email"`, `name="phone"`, `name="csrf"`,
			`lang="en"`, `/theme/default/theme.css`, "Continue to payment"} {
			if !strings.Contains(r.body, want) {
				t.Errorf("page lacks %q", want)
			}
		}
		if strings.Contains(r.body, `name="amount"`) {
			t.Error("a fixed-price link must not ask for an amount")
		}
		if r.header.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(r.header.Get("Cache-Control"), "no-store") || r.header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("headers %v", r.header)
		}
		var visitor *http.Cookie
		for _, c := range (&http.Response{Header: r.header}).Cookies() {
			if c.Name == "pm_visitor" {
				visitor = c
			}
		}
		if visitor == nil || !visitor.HttpOnly || visitor.SameSite != http.SameSiteLaxMode || len(visitor.Value) < 20 {
			t.Errorf("visitor cookie %+v", visitor)
		}
		choose := h.create(func(in *paylinks.CreateInput) { in.Amount, in.MinAmount, in.MaxAmount = 0, 10000, 500000 })
		r = h.do(h.browser(), "GET", "/l/"+choose.Slug, "", nil)
		for _, want := range []string{`name="amount"`, "INR 100.00", "INR 5000.00"} {
			if !strings.Contains(r.body, want) {
				t.Errorf("chooser page lacks %q", want)
			}
		}
	})
}

func TestLinkPageInTheAppsLanguageAndTheme(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		hi := hindiStrings(t)
		l := h.create(nil)
		if err := h.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "customer_language", json.RawMessage(`"hi"`)); err != nil {
			t.Fatal(err)
		}
		if err := h.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "theme", json.RawMessage(`"minimal-dark"`)); err != nil {
			t.Fatal(err)
		}
		r := h.do(h.browser(), "GET", "/l/"+l.Slug, "", nil)
		for _, want := range []string{hi["link.name"], hi["link.continue"], `lang="hi"`, `/theme/minimal-dark/theme.css`} {
			if !strings.Contains(r.body, want) {
				t.Errorf("page lacks %q", want)
			}
		}
		// ?lang= chooses among installed catalogs; anything else is ignored.
		if err := h.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "customer_language", json.RawMessage(`"en"`)); err != nil {
			t.Fatal(err)
		}
		if r := h.do(h.browser(), "GET", "/l/"+l.Slug+"?lang=hi", "", nil); !strings.Contains(r.body, hi["link.continue"]) {
			t.Error("?lang=hi ignored")
		}
		if r := h.do(h.browser(), "GET", "/l/"+l.Slug+"?lang=%3Cscript%3E", "", nil); !strings.Contains(r.body, "Continue to payment") || strings.Contains(r.body, "<script>") {
			t.Error("an unknown ?lang= must be ignored")
		}
	})
}

func TestLinkPageForLinksThatCannotTakePayments(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		hi := hindiStrings(t)
		off := h.create(nil)
		timed := h.create(func(in *paylinks.CreateInput) { in.ExpiresAt = h.now.Add(time.Hour) })
		if _, err := h.links.Deactivate(ctx, apiActor, h.scope, off.ID); err != nil {
			t.Fatal(err)
		}
		h.advance(2 * time.Hour)
		c := h.browser()
		for _, tc := range []struct {
			name, path string
			status     int
			want       string
		}{
			{"unknown", "/l/nosuchslug12", 404, "Link not valid"},
			{"deactivated", "/l/" + off.Slug, 410, "no longer active"},
			{"expired", "/l/" + timed.Slug, 410, "has expired"},
			{"in Hindi", "/l/" + off.Slug + "?lang=hi", 410, hi["link.inactive.title"]},
		} {
			r := h.do(c, "GET", tc.path, "", nil)
			if r.status != tc.status || !strings.Contains(r.body, tc.want) || strings.Contains(r.body, `name="csrf"`) {
				t.Errorf("%s: %d %s", tc.name, r.status, r.body)
			}
		}
		// POSTs are refused the same way, and create nothing.
		for _, slug := range []string{off.Slug, timed.Slug} {
			r := h.do(c, "POST", "/l/"+slug, "", url.Values{"csrf": {"x"}, "name": {"A"}, "email": {"a@example.com"}})
			if r.status == 303 || r.status == 200 {
				t.Errorf("POST to %s: %d", slug, r.status)
			}
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments`).Scan(&n) })
		if n != 0 {
			t.Errorf("%d payments created", n)
		}
	})
}

func TestSubmittingTheFormCreatesAPaymentAndSendsTheCustomerToPay(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(func(in *paylinks.CreateInput) { in.Metadata = map[string]string{"sku": "c1"} })
		r := h.submit(h.browser(), l.Slug, "", asha)
		loc, _ := url.Parse(r.header.Get("Location"))
		if r.status != 303 || loc == nil || !strings.HasPrefix(loc.Path, "/pay/pay_") || loc.Query().Get("t") == "" || loc.Host != "" {
			t.Fatalf("submit: %d %q\n%s", r.status, r.header.Get("Location"), r.body)
		}
		id := strings.TrimPrefix(loc.Path, "/pay/")
		p, err := h.core.GetPaymentByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Metadata["payment_link_id"] != l.ID || p.Metadata["sku"] != "c1" || p.Amount != 49900 || p.Status != "created" {
			t.Fatalf("payment %+v", p)
		}
		var c store.Customer
		_ = db.Read(ctx, func(q store.Querier) (err error) { c, err = store.GetCustomer(ctx, q, p.CustomerID); return })
		if c.Name != "Asha K" || c.Email != "asha@example.com" || c.Phone != "+91 98765 43210" {
			t.Errorf("customer %+v", c)
		}
		if r.header.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(r.header.Get("Cache-Control"), "no-store") {
			t.Errorf("the redirect carries the payment secret and needs no-referrer and no-store: %v", r.header)
		}
	})
}

func TestChosenAmountsAndLanguageSurviveTheRedirect(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(func(in *paylinks.CreateInput) { in.Amount, in.MinAmount, in.MaxAmount = 0, 10000, 500000 })
		who := func(amount string) url.Values {
			f := url.Values{"amount": {amount}}
			for k, v := range asha {
				f[k] = v
			}
			return f
		}
		r := h.submit(h.browser(), l.Slug, "?lang=hi", who("1,250.50"))
		loc, _ := url.Parse(r.header.Get("Location"))
		if r.status != 303 || loc.Query().Get("lang") != "hi" {
			t.Fatalf("submit: %d %q", r.status, r.header.Get("Location"))
		}
		p, _ := h.core.GetPaymentByID(ctx, strings.TrimPrefix(loc.Path, "/pay/"))
		if p.Amount != 125050 {
			t.Errorf("amount = %d, want 125050", p.Amount)
		}
		hi := hindiStrings(t)
		for _, amount := range []string{"50", "5000.01", "abc", "", "-5", "10.999", "1e3"} {
			r := h.submit(h.browser(), l.Slug, "", who(amount))
			if r.status != 400 || !strings.Contains(r.body, `class="error"`) || amount != "" && !strings.Contains(r.body, `value="`+amount+`"`) {
				t.Errorf("amount %q: %d %s", amount, r.status, r.body)
			}
		}
		r = h.submit(h.browser(), l.Slug, "?lang=hi", who("50"))
		if r.status != 400 || !strings.Contains(r.body, strings.SplitN(hi["link.err.amount_range"], "%s", 2)[0]) {
			t.Errorf("Hindi range error: %d %s", r.status, r.body)
		}
	})
}

func TestFormErrorsKeepWhatWasTyped(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(nil)
		cases := map[string]struct {
			mod  func(url.Values)
			want string
		}{
			"no name":   {func(f url.Values) { f.Set("name", " ") }, "Please enter your name."},
			"bad email": {func(f url.Values) { f.Set("email", "nope") }, "valid email address"},
			"bad phone": {func(f url.Values) { f.Set("phone", "call me") }, "valid phone number"},
		}
		for name, tc := range cases {
			f := url.Values{"name": {"Asha K"}, "email": {"asha@example.com"}, "phone": {"98765 43210"}}
			tc.mod(f)
			r := h.submit(h.browser(), l.Slug, "", f)
			if r.status != 400 || !strings.Contains(r.body, tc.want) || !strings.Contains(r.body, `name="csrf"`) {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
			for _, kept := range []string{f.Get("name"), f.Get("email")} {
				if strings.TrimSpace(kept) != "" && !strings.Contains(r.body, `value="`+strings.TrimSpace(kept)+`"`) {
					t.Errorf("%s: the form lost %q", name, kept)
				}
			}
		}
		hi := hindiStrings(t)
		f := url.Values{"name": {""}, "email": {"asha@example.com"}}
		if r := h.submit(h.browser(), l.Slug, "?lang=hi", f); !strings.Contains(r.body, hi["link.err.name"]) {
			t.Errorf("Hindi error missing: %s", r.body)
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM payments`).Scan(&n) })
		if n != 0 {
			t.Errorf("%d payments created from rejected forms", n)
		}
	})
}

func TestPostsNeedTheFormsCSRFToken(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(nil)
		other := h.create(nil)
		c := h.browser()
		form := h.do(c, "GET", "/l/"+l.Slug, "", nil)
		tok := csrfRE.FindStringSubmatch(form.body)[1]
		post := func(c *http.Client, slug, token string) int {
			f := url.Values{"name": {"A"}, "email": {"a@example.com"}}
			if token != "" {
				f.Set("csrf", token)
			}
			return h.do(c, "POST", "/l/"+slug, "", f).status
		}
		if got := post(c, l.Slug, ""); got != 403 {
			t.Errorf("no token: %d", got)
		}
		if got := post(c, l.Slug, "00"+tok); got != 403 {
			t.Errorf("bad token: %d", got)
		}
		if got := post(c, other.Slug, tok); got != 403 {
			t.Errorf("a token of another link: %d", got)
		}
		if got := post(h.browser(), l.Slug, tok); got != 403 {
			t.Errorf("a token without the browser's cookie: %d", got)
		}
		if got := post(c, l.Slug, tok); got != 303 {
			t.Errorf("the right token: %d", got)
		}
	})
}

func TestSingleUseLinkTurnsAwayASecondVisitor(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		l := h.create(func(in *paylinks.CreateInput) { in.SingleUse = true })
		a, b := h.browser(), h.browser()
		if r := h.submit(a, l.Slug, "", asha); r.status != 303 {
			t.Fatalf("first visitor: %d %s", r.status, r.body)
		}
		r := h.submit(b, l.Slug, "", asha)
		if r.status != 409 || !strings.Contains(r.body, "being used right now") {
			t.Fatalf("second visitor: %d %s", r.status, r.body)
		}
		if r := h.submit(a, l.Slug, "", asha); r.status != 303 {
			t.Fatalf("first visitor again: %d %s", r.status, r.body)
		}
	})
}

func paylinksWithLimit(e *env, perMinute int) *paylinks.Service {
	return paylinks.New(paylinks.Options{DB: e.db, Core: e.core, BaseURL: baseURL, Now: e.clock, StartsPerMinute: perMinute,
		Emitter: events.EmitterFunc(func(context.Context, store.Tx, events.Event) error { return nil })})
}

var org = core.OrgScope{OrgID: store.DefaultOrgID}

func TestStartRateLimitShowsAWaitPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.links = paylinksWithLimit(e, 2)
		rend, _ := web.NewRenderer(web.Embedded())
		h := paylinks.NewHosted(paylinks.HostedOptions{Service: e.links, Core: e.core, Renderer: rend, Settings: e.set, CSRFKey: []byte("0123456789abcdef0123456789abcdef"), Now: e.clock})
		mux := http.NewServeMux()
		mux.HandleFunc("GET /l/{slug}", h.Show)
		mux.HandleFunc("POST /l/{slug}", h.Submit)
		srv := httptest.NewServer(mux)
		defer srv.Close()
		l := e.create(nil)
		he := &hostedEnv{env: e, srv: srv}
		var last page
		for i := 0; i < 3; i++ {
			last = he.submit(he.browser(), l.Slug, "", asha)
		}
		if last.status != 429 || last.header.Get("Retry-After") == "" || !strings.Contains(last.body, "Please wait") {
			t.Fatalf("third submit: %d %v %s", last.status, last.header, last.body)
		}
	})
}

func TestACustomDomainServesOnlyItsOwnAppsLinks(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		h := newHosted(t, db)
		if _, err := h.core.AddDomain(ctx, audit.System, org, "app_1", "pay.shop.example.org"); err != nil {
			t.Fatal(err)
		}
		mine := h.create(nil)
		theirs := h.create(func(in *paylinks.CreateInput) { in.Scope.AppID = "app_2" })
		c := h.browser()
		if r := h.do(c, "GET", "/l/"+mine.Slug, "pay.shop.example.org", nil); r.status != 200 || !strings.Contains(r.body, "Course fee") {
			t.Errorf("own link on own domain: %d", r.status)
		}
		if r := h.do(c, "GET", "/l/"+theirs.Slug, "pay.shop.example.org", nil); r.status != 404 {
			t.Errorf("another app's link on this domain: %d", r.status)
		}
		// The base host serves every app's links.
		if r := h.do(c, "GET", "/l/"+theirs.Slug, "", nil); r.status != 200 {
			t.Errorf("base host: %d", r.status)
		}
		if r := h.submit(h.browser(), mine.Slug, "", asha); r.status != 303 {
			t.Errorf("submit on base host: %d", r.status)
		}
	})
}

func TestDomainsListsLinkPagesAsCustomerRoutes(t *testing.T) {
	if !domains.IsCustomerRoute("/l/abcdefghijkl") {
		t.Fatal("/l/ must be served on custom domains")
	}
}
