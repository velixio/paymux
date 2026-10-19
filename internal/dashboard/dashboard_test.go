package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/alertcenter"
	"github.com/paymux/paymux/internal/auth"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/disputes"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/paylinks"
	"github.com/paymux/paymux/internal/reconcile"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/web"
)

var ctx = context.Background()

const goodPW = "correct horse battery"

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
	t      *testing.T
	db     *store.DB
	box    *secrets.Box
	core   *core.Service
	set    *settings.Service
	out    *outbound.Service
	inv    *invoice.Service
	disp   *disputes.Service
	links  *paylinks.Service
	alerts *alertcenter.Service
	rec    *reconcile.Service
	conn   *connectors.Service
	mail   *recMailer
	q      *jobs.Queue
	d      *Dashboard
	srv    *httptest.Server
	clk    *clock
	c      *http.Client
}

type envOpt func(*Options)

func newEnv(t *testing.T, db *store.DB, opts ...envOpt) *env {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	rend, err := web.NewRenderer(web.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_800_000_000, 0).UTC()}
	base, _ := url.Parse("http://localhost:8080")
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	q := jobs.New(db, jobs.Options{Clock: clk.Now, Logger: slog.New(slog.DiscardHandler)})
	out := outbound.New(outbound.Options{DB: db, Queue: q, Box: box, AllowPrivate: true, Now: clk.Now, Logger: slog.New(slog.DiscardHandler)})
	out.Register()
	core.RegisterJobs(q)
	rec := &recMailer{}
	inv := invoice.New(invoice.Options{DB: db, Box: box, Settings: set, Jobs: q, Emitter: out, Mailer: rec, BaseURL: "https://pay.example.com", Now: clk.Now,
		IsMerchantOfRecord: func(string) bool { return false }})
	inv.RegisterJobs(q)
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now})
	svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: q, Emitter: out, Now: clk.Now,
		OnPaymentSucceeded: inv.OnPaymentSucceeded, OnRefundSucceeded: inv.OnRefundSucceeded})
	alertCenter := alertcenter.New(db, clk.Now)
	links := paylinks.New(paylinks.Options{DB: db, Core: svc, Emitter: out, Settings: set, BaseURL: "https://pay.example.com", Now: clk.Now})
	disp := disputes.New(disputes.Options{DB: db, Settings: set, Emitter: out, Credit: inv, CutoffErr: invoice.ErrCreditNoteCutoff, Conns: conns, Now: clk.Now})
	reco := reconcile.New(reconcile.Options{DB: db, Core: svc, Conns: conns, Settings: set, Loc: time.UTC, Logger: slog.New(slog.DiscardHandler), Now: clk.Now})
	if err := reco.Register(q); err != nil {
		t.Fatal(err)
	}
	o := Options{DB: db, Box: box, Renderer: rend, MasterKey: make([]byte, 32), BaseURL: base,
		Logger: slog.New(slog.DiscardHandler), Now: clk.Now, Core: svc, Settings: set, Outbound: out, Invoice: inv, Connectors: conns, Disputes: disp, Recon: reco, Jobs: q, PaymentLinks: links, Alerts: alertCenter, PageSize: 2}
	for _, f := range opts {
		f(&o)
	}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/dashboard/", d)
	mux.Handle("/setup", d.SetupHandler())
	e := &env{t: t, db: db, box: box, core: svc, set: set, out: out, inv: inv, disp: disp, links: links, alerts: alertCenter, rec: reco, conn: conns, mail: rec, q: q, d: d, srv: httptest.NewServer(mux), clk: clk}
	t.Cleanup(e.srv.Close)
	e.c = e.client()
	return e
}

func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(c *http.Client, path string) (*http.Response, string) {
	e.t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) post(c *http.Client, path string, form url.Values) (*http.Response, string) {
	e.t.Helper()
	resp, err := c.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfOf(t *testing.T, body string) string {
	t.Helper()
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf field in page:\n%s", body)
	}
	return m[1]
}

// totpFor computes the RFC 6238 code (SHA-1, 6 digits) for the step at time at.
func totpFor(secret string, at time.Time, stepOffset int64) string {
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var ctr [8]byte
	binary.BigEndian.PutUint64(ctr[:], uint64(at.Unix()/30+stepOffset))
	m := hmac.New(sha1.New, raw)
	m.Write(ctr[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code)
}

// seedUser creates a user. When enrolled, it also has a confirmed TOTP secret
// (returned); otherwise the secret is "" and the user must enrol at first login.
func (e *env) seedUser(email, role string, enrolled bool) (id, secret string) {
	e.t.Helper()
	hash, err := auth.HashPassword(goodPW)
	if err != nil {
		e.t.Fatal(err)
	}
	id = "usr_" + strings.Split(email, "@")[0]
	u := store.UserRow{ID: id, Email: email, PasswordHash: hash, TOTPLastStep: -1, Role: role, CreatedAt: e.clk.Now()}
	if err := e.db.InsertUser(ctx, u); err != nil {
		e.t.Fatal(err)
	}
	if enrolled {
		secret, _, _ = auth.NewTOTPSecret("Paymux", email)
		enc, _ := e.box.Seal([]byte(secret), []byte("users.totp_secret:"+id))
		if err := e.db.SetUserTOTP(ctx, id, enc); err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.db.AdvanceTOTPStep(ctx, id, 0); err != nil { // confirmed long ago
			e.t.Fatal(err)
		}
	}
	return id, secret
}

// passwordStep submits the login form; it returns the response.
func (e *env) passwordStep(c *http.Client, email, pw string) (*http.Response, string) {
	e.t.Helper()
	_, page := e.get(c, "/dashboard/login")
	return e.post(c, "/dashboard/login", url.Values{"email": {email}, "password": {pw}, "csrf": {csrfOf(e.t, page)}})
}

// login runs the full password + TOTP flow and returns the TOTP page body.
func (e *env) login(c *http.Client, email, secret string) *http.Response {
	e.t.Helper()
	resp, body := e.passwordStep(c, email, goodPW)
	if resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/dashboard/login/totp") {
		e.t.Fatalf("password step: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	_, page := e.get(c, "/dashboard/login/totp")
	resp, body = e.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrfOf(e.t, page)}})
	if resp.StatusCode != 303 {
		e.t.Fatalf("totp step: %d\n%s", resp.StatusCode, body)
	}
	return resp
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == "pm_session" {
			return c
		}
	}
	return nil
}

func (e *env) authedCSRF(c *http.Client) string {
	e.t.Helper()
	resp, body := e.get(c, "/dashboard/users")
	if resp.StatusCode != 200 {
		e.t.Fatalf("users page: %d", resp.StatusCode)
	}
	return csrfOf(e.t, body)
}

func TestLoginWithPasswordAndTOTP(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		resp := e.login(e.c, "alice@example.com", secret)
		if resp.Header.Get("Location") != "/dashboard/" {
			t.Fatalf("redirect %q", resp.Header.Get("Location"))
		}
		ck := sessionCookie(resp)
		if ck == nil || ck.Value == "" || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Domain != "" || ck.Secure {
			t.Fatalf("session cookie %+v", ck)
		}
		if strings.Contains(ck.Value, "usr_") {
			t.Error("cookie must be an opaque token")
		}
		// only the token hash is stored
		_, _, err := db.SessionByTokenHash(ctx, auth.HashToken(ck.Value))
		if err != nil {
			t.Fatalf("session not stored by hash: %v", err)
		}
		r, body := e.get(e.c, "/dashboard/payments")
		if r.StatusCode != 200 {
			t.Fatalf("payments: %d", r.StatusCode)
		}
		for _, item := range []string{"Overview", "Payments", "Subscriptions", "Disputes", "Invoices", "Reports", "Apps", "Providers", "Settings", "Audit", "Users"} {
			if !strings.Contains(body, ">"+item+"<") {
				t.Errorf("nav missing %s", item)
			}
		}
		if strings.Contains(body, "2FA disabled") {
			t.Error("dev banner shown in normal mode")
		}
		if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Frame-Options") != "DENY" || r.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("security headers %v", r.Header)
		}
		r, _ = e.get(e.c, "/dashboard/")
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/overview" {
			t.Fatalf("root: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
	})
}

func TestSecureCookieFollowsBaseURL(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		https, _ := url.Parse("https://pay.example.com")
		e := newEnv(t, db, func(o *Options) { o.BaseURL = https })
		_, secret := e.seedUser("alice@example.com", "admin", true)
		resp := e.login(e.c, "alice@example.com", secret)
		ck := sessionCookie(resp)
		if ck == nil || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Domain != "" {
			t.Fatalf("cookie %+v", ck)
		}
	})
}

func TestUnauthenticatedRequestsRedirectToLogin(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for _, p := range []string{"/dashboard/", "/dashboard/payments", "/dashboard/users", "/dashboard/settings"} {
			r, _ := e.get(e.c, p)
			if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
				t.Errorf("%s: %d %s", p, r.StatusCode, r.Header.Get("Location"))
			}
		}
		r, _ := e.post(e.c, "/dashboard/users", url.Values{"email": {"x@example.com"}})
		if r.StatusCode == 200 || r.StatusCode == 303 && r.Header.Get("Location") != "/dashboard/login" {
			t.Errorf("anonymous POST not rejected: %d", r.StatusCode)
		}
	})
}

func TestWrongPasswordIsGenericAndCreatesNoSession(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		r1, b1 := e.passwordStep(e.c, "alice@example.com", "wrong password here")
		r2, b2 := e.passwordStep(e.c, "nobody@example.com", "wrong password here")
		for _, r := range []*http.Response{r1, r2} {
			if r.StatusCode != 200 || sessionCookie(r) != nil || strings.Contains(r.Header.Get("Location"), "totp") {
				t.Fatalf("status %d", r.StatusCode)
			}
		}
		if !strings.Contains(b1, "Invalid email or password") || !strings.Contains(b2, "Invalid email or password") {
			t.Fatal("no generic error")
		}
		// Identical page apart from echoed email and per-response CSRF/style nonces.
		strip := func(s, email string) string {
			s = regexp.MustCompile(`data-style-nonce="[A-Za-z0-9_-]{32}"`).ReplaceAllString(s, `data-style-nonce="NONCE"`)
			return strings.ReplaceAll(csrfRE.ReplaceAllString(s, ""), email, "EMAIL")
		}
		if strip(b1, "alice@example.com") != strip(b2, "nobody@example.com") {
			t.Error("response differs for a known account")
		}
		// the TOTP page cannot be reached without the password step
		if r, _ := e.get(e.c, "/dashboard/login/totp"); r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
			t.Errorf("totp page without ticket: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
	})
}

func TestWrongTOTPRejected(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		e.passwordStep(e.c, "alice@example.com", goodPW)
		_, page := e.get(e.c, "/dashboard/login/totp")
		bad := totpFor(secret, e.clk.Now(), 5) // outside the accepted window
		r, body := e.post(e.c, "/dashboard/login/totp", url.Values{"code": {bad}, "csrf": {csrfOf(t, page)}})
		if r.StatusCode != 200 || sessionCookie(r) != nil || !strings.Contains(body, "Invalid code") {
			t.Fatalf("%d %q", r.StatusCode, body)
		}
	})
}

func TestTOTPReplayRejected(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		first := e.login(e.c, "alice@example.com", secret)
		if sessionCookie(first) == nil {
			t.Fatal("no session")
		}
		// a second client replays the code that was just used
		c2 := e.client()
		if r, _ := e.passwordStep(c2, "alice@example.com", goodPW); r.StatusCode != 303 {
			t.Fatalf("password step %d", r.StatusCode)
		}
		_, page := e.get(c2, "/dashboard/login/totp")
		used := totpFor(secret, e.clk.Now(), 0)
		r, _ := e.post(c2, "/dashboard/login/totp", url.Values{"code": {used}, "csrf": {csrfOf(t, page)}})
		if r.StatusCode != 200 || sessionCookie(r) != nil {
			t.Fatalf("replayed code accepted: %d", r.StatusCode)
		}
		// an older step is rejected too, a newer one is fine
		r, _ = e.post(c2, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), -1)}, "csrf": {csrfOf(t, page)}})
		if sessionCookie(r) != nil {
			t.Fatal("older step accepted")
		}
		r, _ = e.post(c2, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 1)}, "csrf": {csrfOf(t, page)}})
		if r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Fatalf("fresh code rejected: %d", r.StatusCode)
		}
	})
}

func TestLockoutAfterFiveFailures(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		e.seedUser("bob@example.com", "viewer", true)
		for i := 0; i < 5; i++ {
			r, _ := e.passwordStep(e.c, "alice@example.com", "wrong password here")
			if r.StatusCode != 200 {
				t.Fatalf("attempt %d: %d", i+1, r.StatusCode)
			}
		}
		// the correct password is refused while locked out
		r, body := e.passwordStep(e.c, "alice@example.com", goodPW)
		if r.StatusCode != 429 || !strings.Contains(body, "Too many") {
			t.Fatalf("locked out login: %d", r.StatusCode)
		}
		// another account (same IP) is not affected
		if r, _ := e.passwordStep(e.client(), "bob@example.com", goodPW); r.StatusCode != 303 {
			t.Fatalf("bob: %d", r.StatusCode)
		}
		// case-insensitive email cannot dodge the limit
		if r, _ := e.passwordStep(e.client(), "ALICE@example.com", goodPW); r.StatusCode != 429 {
			t.Fatalf("uppercase email: %d", r.StatusCode)
		}
		e.clk.Add(15*time.Minute + time.Second)
		e.login(e.client(), "alice@example.com", secret)
	})
}

func TestTOTPFailuresCountTowardsLockout(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		e.passwordStep(e.c, "alice@example.com", goodPW)
		_, page := e.get(e.c, "/dashboard/login/totp")
		tok := csrfOf(t, page)
		for i := 0; i < 5; i++ {
			e.post(e.c, "/dashboard/login/totp", url.Values{"code": {"000000"}, "csrf": {tok}})
		}
		r, _ := e.post(e.c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {tok}})
		if r.StatusCode != 429 || sessionCookie(r) != nil {
			t.Fatalf("brute force of the TOTP step not stopped: %d", r.StatusCode)
		}
	})
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLimiter(5, 15*time.Minute)
	for i := 0; i < 5; i++ {
		if l.locked("a|1.1.1.1", now) {
			t.Fatalf("locked after %d", i)
		}
		l.fail("a|1.1.1.1", now)
	}
	if !l.locked("a|1.1.1.1", now) || l.locked("a|2.2.2.2", now) || l.locked("b|1.1.1.1", now) {
		t.Fatal("key scoping wrong")
	}
	if l.locked("a|1.1.1.1", now.Add(15*time.Minute+time.Second)) {
		t.Fatal("window not sliding")
	}
	l.fail("a|1.1.1.1", now)
	l.reset("a|1.1.1.1")
	if l.size() != 0 {
		t.Fatalf("reset left %d", l.size())
	}
}

func TestCSRFRequiredOnEveryPost(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		// login form
		r, _ := e.post(e.c, "/dashboard/login", url.Values{"email": {"alice@example.com"}, "password": {goodPW}})
		if r.StatusCode != 403 {
			t.Errorf("login without csrf: %d", r.StatusCode)
		}
		r, _ = e.post(e.c, "/dashboard/login", url.Values{"email": {"alice@example.com"}, "password": {goodPW}, "csrf": {"forged"}})
		if r.StatusCode != 403 {
			t.Errorf("login with forged csrf: %d", r.StatusCode)
		}
		// authenticated forms
		_, secret := e.seedUser("carol@example.com", "admin", true)
		e.login(e.c, "carol@example.com", secret)
		good := e.authedCSRF(e.c)
		for name, form := range map[string]url.Values{
			"create":  {"email": {"n@example.com"}, "role": {"viewer"}, "password": {goodPW}},
			"role":    {"role": {"viewer"}},
			"disable": {"action": {"disable"}},
			"logout":  {},
		} {
			path := "/dashboard/users"
			switch name {
			case "role":
				path += "/usr_alice/role"
			case "disable":
				path += "/usr_alice/disable"
			case "logout":
				path = "/dashboard/logout"
			}
			if r, _ := e.post(e.c, path, form); r.StatusCode != 403 {
				t.Errorf("%s without csrf: %d", name, r.StatusCode)
			}
			form.Set("csrf", good+"x")
			if r, _ := e.post(e.c, path, form); r.StatusCode != 403 {
				t.Errorf("%s with wrong csrf: %d", name, r.StatusCode)
			}
		}
		if n, _ := db.CountUsers(ctx); n != 2 {
			t.Fatalf("a rejected POST changed state (users=%d)", n)
		}
		// a CSRF token from another session does not work either
		c2 := e.client()
		_, s2 := e.seedUser("dave@example.com", "admin", true)
		e.login(c2, "dave@example.com", s2)
		other := e.authedCSRF(c2)
		if r, _ := e.post(e.c, "/dashboard/logout", url.Values{"csrf": {other}}); r.StatusCode != 403 {
			t.Errorf("csrf from another session accepted: %d", r.StatusCode)
		}
	})
}

func TestViewerAndOperatorCannotManageUsers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.seedUser("root@example.com", "admin", true)
		for _, role := range []string{"viewer", "operator"} {
			email := role + "@example.com"
			id, secret := e.seedUser(email, role, true)
			c := e.client()
			e.login(c, email, secret)
			r, body := e.get(c, "/dashboard/users")
			if r.StatusCode != 200 || strings.Contains(body, `action="/dashboard/users"`) {
				t.Fatalf("%s: list %d / create form visible", role, r.StatusCode)
			}
			tok := csrfOfPage(t, e, c)
			if r, _ := e.post(c, "/dashboard/users", url.Values{"email": {"x" + role + "@example.com"}, "role": {"admin"}, "password": {goodPW}, "csrf": {tok}}); r.StatusCode != 403 {
				t.Errorf("%s create: %d", role, r.StatusCode)
			}
			if r, _ := e.post(c, "/dashboard/users/"+id+"/role", url.Values{"role": {"admin"}, "csrf": {tok}}); r.StatusCode != 403 {
				t.Errorf("%s self-promotion: %d", role, r.StatusCode)
			}
			if r, _ := e.post(c, "/dashboard/users/usr_root/disable", url.Values{"action": {"disable"}, "csrf": {tok}}); r.StatusCode != 403 {
				t.Errorf("%s disable: %d", role, r.StatusCode)
			}
		}
		if n, _ := db.CountUsers(ctx); n != 3 {
			t.Fatalf("users=%d", n)
		}
	})
}

// csrfOfPage reads the CSRF token from the logout form in the nav shell.
func csrfOfPage(t *testing.T, e *env, c *http.Client) string {
	t.Helper()
	_, body := e.get(c, "/dashboard/payments")
	return csrfOf(t, body)
}

func TestAdminManagesUsers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("root@example.com", "admin", true)
		e.login(e.c, "root@example.com", secret)
		tok := e.authedCSRF(e.c)

		r, body := e.post(e.c, "/dashboard/users", url.Values{"email": {"New.Person@Example.com"}, "role": {"operator"}, "password": {goodPW}, "csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("create: %d\n%s", r.StatusCode, body)
		}
		u, err := db.UserByEmail(ctx, "new.person@example.com")
		if err != nil || u.Role != "operator" || u.Disabled || u.PasswordHash == goodPW || u.TOTPSecretEnc != nil {
			t.Fatalf("created user %+v %v", u, err)
		}
		if _, body = e.get(e.c, "/dashboard/users"); !strings.Contains(body, "new.person@example.com") {
			t.Error("not listed")
		}
		// validation
		for name, form := range map[string]url.Values{
			"duplicate":     {"email": {"new.person@example.com"}, "role": {"viewer"}, "password": {goodPW}},
			"weak password": {"email": {"w@example.com"}, "role": {"viewer"}, "password": {"short"}},
			"bad role":      {"email": {"w@example.com"}, "role": {"root"}, "password": {goodPW}},
			"bad email":     {"email": {"nope"}, "role": {"viewer"}, "password": {goodPW}},
		} {
			form.Set("csrf", tok)
			if r, _ := e.post(e.c, "/dashboard/users", form); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}

		// role change
		if r, _ := e.post(e.c, "/dashboard/users/"+u.ID+"/role", url.Values{"role": {"viewer"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("role change %d", r.StatusCode)
		}
		if u, _ = db.UserByID(ctx, u.ID); u.Role != "viewer" {
			t.Fatalf("role %s", u.Role)
		}
		// disable revokes sessions and blocks login
		_ = db.InsertSession(ctx, store.SessionRow{ID: "ses_x", UserID: u.ID, TokenHash: "h", CSRFKey: []byte{1}, CreatedAt: e.clk.Now(),
			LastSeenAt: e.clk.Now(), ExpiresAt: e.clk.Now().Add(time.Hour), IP: "x", UserAgent: "x"})
		if r, _ := e.post(e.c, "/dashboard/users/"+u.ID+"/disable", url.Values{"action": {"disable"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("disable %d", r.StatusCode)
		}
		if _, _, err := db.SessionByTokenHash(ctx, "h"); err == nil {
			t.Error("sessions of a disabled user survive")
		}
		if r, body := e.passwordStep(e.client(), "new.person@example.com", goodPW); r.StatusCode != 200 || !strings.Contains(body, "Invalid email or password") {
			t.Errorf("disabled user could log in: %d", r.StatusCode)
		}
		if r, _ := e.post(e.c, "/dashboard/users/"+u.ID+"/disable", url.Values{"action": {"enable"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("enable %d", r.StatusCode)
		}
		if u, _ = db.UserByID(ctx, u.ID); u.Disabled {
			t.Error("still disabled")
		}
		// an admin cannot lock themselves out
		if r, _ := e.post(e.c, "/dashboard/users/usr_root/disable", url.Values{"action": {"disable"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("self-disable %d", r.StatusCode)
		}
		if r, _ := e.post(e.c, "/dashboard/users/usr_root/role", url.Values{"role": {"viewer"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("self-demote %d", r.StatusCode)
		}
		if r, _ := e.post(e.c, "/dashboard/users/usr_missing/role", url.Values{"role": {"viewer"}, "csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("missing user %d", r.StatusCode)
		}
	})
}

func TestFirstLoginEnrolsTOTP(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, _ := e.seedUser("newbie@example.com", "viewer", false)
		resp, _ := e.passwordStep(e.c, "newbie@example.com", goodPW)
		if resp.StatusCode != 303 {
			t.Fatalf("password step %d", resp.StatusCode)
		}
		_, page := e.get(e.c, "/dashboard/login/totp")
		uriRE := regexp.MustCompile(`otpauth://totp/[^<"\s]+`)
		secRE := regexp.MustCompile(`<code id="totp-secret">([A-Z2-7]+)</code>`)
		uri, sec := uriRE.FindString(page), secRE.FindStringSubmatch(page)
		if uri == "" || sec == nil {
			t.Fatalf("enrolment page lacks the otpauth URI or secret:\n%s", page)
		}
		secret := sec[1]
		if !strings.Contains(strings.ReplaceAll(uri, "&amp;", "&"), "secret="+secret) {
			t.Error("URI and shown secret differ")
		}
		u, _ := db.UserByID(ctx, id)
		if u.TOTPSecretEnc == nil || strings.Contains(string(u.TOTPSecretEnc), secret) {
			t.Fatal("secret not stored encrypted")
		}
		if pt, err := e.box.Open(u.TOTPSecretEnc, []byte("users.totp_secret:"+id)); err != nil || string(pt) != secret {
			t.Fatalf("stored secret %q %v", pt, err)
		}
		// reloading the page shows the same secret until the first code is accepted
		_, page2 := e.get(e.c, "/dashboard/login/totp")
		if s2 := secRE.FindStringSubmatch(page2); s2 == nil || s2[1] != secret {
			t.Fatal("secret changed on reload")
		}
		r, _ := e.post(e.c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrfOf(t, page2)}})
		if r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Fatalf("confirm: %d", r.StatusCode)
		}
		// once enrolled the secret is never displayed again
		e.clk.Add(2 * time.Minute)
		c2 := e.client()
		e.passwordStep(c2, "newbie@example.com", goodPW)
		_, page3 := e.get(c2, "/dashboard/login/totp")
		if secRE.MatchString(page3) || uriRE.MatchString(page3) {
			t.Fatal("secret shown after enrolment")
		}
	})
}

func TestSessionIdleTimeoutAndAbsoluteLimit(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		e.login(e.c, "alice@example.com", secret)
		ok := func() bool { r, _ := e.get(e.c, "/dashboard/payments"); return r.StatusCode == 200 }

		// activity keeps the session alive past 12h in total
		for i := 0; i < 3; i++ {
			e.clk.Add(11 * time.Hour)
			if !ok() {
				t.Fatalf("session died after %dh of activity", (i+1)*11)
			}
		}
		// idle for more than 12h: gone
		e.clk.Add(12*time.Hour + time.Second)
		r, _ := e.get(e.c, "/dashboard/payments")
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
			t.Fatalf("idle session still valid: %d", r.StatusCode)
		}
		if n := countSessions(t, db); n != 0 {
			t.Fatalf("expired session row kept (%d)", n)
		}

		// continuous activity cannot extend a session beyond the absolute limit
		c2 := e.client()
		e.clk.Add(2 * time.Minute) // fresh TOTP step
		e.login(c2, "alice@example.com", secret)
		for i := 0; i < 8*2; i++ {
			e.clk.Add(11 * time.Hour)
			r, _ = e.get(c2, "/dashboard/payments")
		}
		if r.StatusCode != 303 {
			t.Fatalf("session outlived the absolute limit: %d", r.StatusCode)
		}
	})
}

func countSessions(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	err := db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n) })
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDisabledUserSessionIsRejected(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, secret := e.seedUser("alice@example.com", "admin", true)
		e.login(e.c, "alice@example.com", secret)
		if err := db.SetUserDisabled(ctx, id, true); err != nil { // not via the UI, so sessions remain
			t.Fatal(err)
		}
		if r, _ := e.get(e.c, "/dashboard/payments"); r.StatusCode != 303 {
			t.Fatalf("disabled user kept access: %d", r.StatusCode)
		}
	})
}

func TestDevSkip2FA(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(o *Options) { o.DevSkip2FA = true })
		e.seedUser("alice@example.com", "admin", false) // never enrolled
		_, loginPage := e.get(e.c, "/dashboard/login")
		if !strings.Contains(loginPage, "2FA disabled (development)") {
			t.Error("no banner on the login page")
		}
		resp, _ := e.passwordStep(e.c, "alice@example.com", goodPW)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("dev login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		for _, p := range []string{"/dashboard/payments", "/dashboard/users"} {
			if r, body := e.get(e.c, p); r.StatusCode != 200 || !strings.Contains(body, "2FA disabled (development)") {
				t.Errorf("%s: %d banner missing", p, r.StatusCode)
			}
		}
		// wrong passwords still fail
		if r, _ := e.passwordStep(e.client(), "alice@example.com", "wrong password here"); sessionCookie(r) != nil {
			t.Error("wrong password accepted in dev mode")
		}
	})
}

func TestLogout(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		resp := e.login(e.c, "alice@example.com", secret)
		old := sessionCookie(resp).Value
		tok := csrfOfPage(t, e, e.c)
		r, _ := e.post(e.c, "/dashboard/logout", url.Values{"csrf": {tok}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
			t.Fatalf("logout %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		if ck := sessionCookie(r); ck == nil || ck.MaxAge >= 0 {
			t.Errorf("cookie not cleared: %+v", ck)
		}
		if _, _, err := db.SessionByTokenHash(ctx, auth.HashToken(old)); err == nil {
			t.Fatal("session row survives logout")
		}
		// the old cookie is useless
		c2 := e.client()
		u, _ := url.Parse(e.srv.URL)
		c2.Jar.SetCookies(u, []*http.Cookie{{Name: "pm_session", Value: old, Path: "/dashboard"}})
		if r, _ := e.get(c2, "/dashboard/payments"); r.StatusCode != 303 {
			t.Fatalf("stolen cookie still valid: %d", r.StatusCode)
		}
	})
}

func TestEveryLoginIssuesAFreshToken(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		a := sessionCookie(e.login(e.c, "alice@example.com", secret)).Value
		e.clk.Add(2 * time.Minute)
		b := sessionCookie(e.login(e.client(), "alice@example.com", secret)).Value
		if a == b {
			t.Fatal("session token reused")
		}
	})
}

type recMailer struct {
	mu   sync.Mutex
	sent []notify.Message
	err  error
}

func (m *recMailer) Send(_ context.Context, msg notify.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *recMailer) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.sent) }
