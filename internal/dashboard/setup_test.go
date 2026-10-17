package dashboard

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

const setupPW = "correct horse battery staple"

var apiKeyRE = regexp.MustCompile(`pmx_(test|live)_sk_[A-Za-z0-9]+`)

func (e *env) setupForm(c *http.Client, mod func(url.Values)) (*http.Response, string) {
	e.t.Helper()
	r, page := e.get(c, "/setup")
	if r.StatusCode != 200 {
		e.t.Fatalf("GET /setup: %d %s", r.StatusCode, page)
	}
	f := url.Values{"token": {e.d.SetupToken()}, "email": {"owner@example.com"}, "password": {setupPW}, "password2": {setupPW},
		"org_name": {"Acme Pay"}, "app_name": {""}, "app_slug": {""}, "csrf": {csrfOf(e.t, page)}}
	if mod != nil {
		mod(f)
	}
	return e.post(c, "/setup", f)
}

func (e *env) userCount() int {
	e.t.Helper()
	n, err := e.db.CountUsers(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestSetupIsOnlyOpenWhileThereAreNoUsers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		if e.d.SetupToken() == "" {
			t.Fatal("a hub without users should have a setup token")
		}
		r, body := e.get(e.c, "/setup")
		has(t, body, `name="token"`, `name="email"`, `name="password"`, `name="org_name"`, `name="app_name"`, `name="csrf"`)
		if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("setup page: %d %v", r.StatusCode, r.Header)
		}
		// The login page points to it while it is open.
		_, login := e.get(e.client(), "/dashboard/login")
		has(t, login, `href="/setup"`)

		e.seedUser("someone@example.com", "admin", true)
		for _, m := range []string{"GET", "POST"} {
			req, _ := http.NewRequest(m, e.srv.URL+"/setup", strings.NewReader("token=x"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			res, err := e.c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 404 {
				t.Errorf("%s /setup with users: %d, want 404", m, res.StatusCode)
			}
		}
		_, login = e.get(e.client(), "/dashboard/login")
		lacks(t, login, `/setup`)
		if e.d.SetupToken() != "" {
			t.Error("the token should be gone once a user exists")
		}
	})
}

func TestSetupCreatesAdminOrgAppAndKeysThenEnrolsTwoFactor(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := e.client()
		r, body := e.setupForm(c, func(f url.Values) { f.Set("app_name", "Acme Shop") })
		if r.StatusCode != 200 {
			t.Fatalf("setup: %d\n%s", r.StatusCode, body)
		}
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("a page showing API keys must not be cached: %v", r.Header)
		}
		keys := apiKeyRE.FindAllString(body, -1)
		if len(keys) != 2 || keys[0] == keys[1] {
			t.Fatalf("want one test and one live key on the page, got %v", keys)
		}
		has(t, body, "Acme Shop", "owner@example.com", "otpauth://totp/", `action="/dashboard/login/totp"`)

		// Everything was created.
		u, err := e.db.UserByEmail(ctx, "owner@example.com")
		if err != nil || u.Role != "admin" || u.Disabled {
			t.Fatalf("user %+v %v", u, err)
		}
		var orgName string
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT name FROM orgs WHERE id = ?`, store.DefaultOrgID).Scan(&orgName)
		})
		if orgName != "Acme Pay" {
			t.Errorf("org name = %q", orgName)
		}
		var apps, apiKeys int
		_ = db.Read(ctx, func(q store.Querier) error {
			_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE slug = 'acme-shop'`).Scan(&apps)
			return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&apiKeys)
		})
		if apps != 1 || apiKeys != 2 {
			t.Errorf("apps=%d keys=%d", apps, apiKeys)
		}

		// Enrol 2FA with the secret shown, which signs the admin in.
		secret := regexp.MustCompile(`secret=([A-Z2-7]+)`).FindStringSubmatch(body)
		if secret == nil {
			t.Fatalf("no TOTP secret on the page:\n%s", body)
		}
		resp, out := e.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret[1], e.clk.Now(), 0)}, "csrf": {csrfOf(t, body)}})
		if resp.StatusCode != 303 || sessionCookie(resp) == nil {
			t.Fatalf("enrolment: %d %s", resp.StatusCode, out)
		}
		has(t, e.pageOK(c, "/dashboard/overview"), "Overview")
		// Second use of the one-time page is closed.
		if r, _ := e.get(c, "/setup"); r.StatusCode != 404 {
			t.Errorf("/setup after setup: %d", r.StatusCode)
		}
		if e.d.SetupToken() != "" {
			t.Error("token still valid after setup")
		}
		// Keys and secrets are on the response page only: not in the audit log.
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "", 50)
			return
		})
		var seen []string
		for _, a := range aud {
			seen = append(seen, a.Action)
			blob := a.Action + a.Target + string(a.Before) + string(a.After)
			for _, secretish := range append(keys, setupPW, secret[1]) {
				if strings.Contains(blob, secretish) {
					t.Errorf("audit row %s leaks a secret", a.Action)
				}
			}
		}
		if !strings.Contains(strings.Join(seen, ","), "setup.complete") {
			t.Errorf("setup should be audited, got %v", seen)
		}
	})
}

func TestSetupWithoutAnAppShowsNoKeys(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		r, body := e.setupForm(e.client(), nil)
		if r.StatusCode != 200 || len(apiKeyRE.FindAllString(body, -1)) != 0 {
			t.Fatalf("%d %s", r.StatusCode, body)
		}
		var n int
		_ = db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n) })
		if n != 0 || e.userCount() != 1 {
			t.Errorf("apps=%d users=%d", n, e.userCount())
		}
	})
}

func TestSetupRejectsBadInputWithoutCreatingAnything(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := e.client()
		cases := map[string]struct {
			mod    func(url.Values)
			status int
		}{
			"wrong token":         {func(f url.Values) { f.Set("token", "nope") }, 403},
			"empty token":         {func(f url.Values) { f.Set("token", "") }, 403},
			"missing csrf":        {func(f url.Values) { f.Del("csrf") }, 403},
			"weak password":       {func(f url.Values) { f.Set("password", "short"); f.Set("password2", "short") }, 400},
			"passwords differ":    {func(f url.Values) { f.Set("password2", setupPW+"x") }, 400},
			"bad email":           {func(f url.Values) { f.Set("email", "nobody") }, 400},
			"org name too long":   {func(f url.Values) { f.Set("org_name", strings.Repeat("x", 101)) }, 400},
			"bad slug":            {func(f url.Values) { f.Set("app_name", "Shop"); f.Set("app_slug", "Not A Slug") }, 400},
			"slug without a name": {func(f url.Values) { f.Set("app_slug", "shop") }, 400},
		}
		for name, tc := range cases {
			r, body := e.setupForm(c, tc.mod)
			if r.StatusCode != tc.status {
				t.Errorf("%s: %d, want %d", name, r.StatusCode, tc.status)
			}
			if apiKeyRE.MatchString(body) || strings.Contains(body, setupPW) {
				t.Errorf("%s: the response leaks secrets", name)
			}
			if tc.status == 400 && !strings.Contains(body, `class="error"`) {
				t.Errorf("%s: no error shown", name)
			}
		}
		if e.userCount() != 0 {
			t.Fatal("a rejected setup created a user")
		}
		if e.d.SetupToken() == "" {
			t.Error("a rejected setup must not burn the token")
		}
		// Rejected forms keep what was typed, except secrets.
		_, body := e.setupForm(c, func(f url.Values) { f.Set("email", "keep@example.com"); f.Set("password2", "different") })
		has(t, body, "keep@example.com")
		lacks(t, body, setupPW)
		// And it still works after all that.
		if r, body := e.setupForm(c, nil); r.StatusCode != 200 {
			t.Fatalf("valid setup after mistakes: %d %s", r.StatusCode, body)
		}
	})
}

func TestSetupGuessesAreRateLimited(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := e.client()
		for i := 0; i < 5; i++ {
			if r, _ := e.setupForm(c, func(f url.Values) { f.Set("token", "guess") }); r.StatusCode != 403 {
				t.Fatalf("guess %d: %d", i, r.StatusCode)
			}
		}
		// Locked out: even the right token is refused now.
		if r, _ := e.setupForm(c, nil); r.StatusCode != 429 {
			t.Fatalf("after 5 wrong tokens: %d, want 429", r.StatusCode)
		}
		if e.userCount() != 0 {
			t.Error("a locked-out request created a user")
		}
	})
}

func TestTwoSimultaneousSetupsCreateOneAdmin(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		var wg sync.WaitGroup
		codes := make([]int, 4)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := e.client()
				// No t.Fatal in here: a late request legitimately finds /setup closed.
				r, page := e.get(c, "/setup")
				m := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
				if r.StatusCode != 200 || m == nil {
					codes[i] = r.StatusCode
					return
				}
				res, _ := e.post(c, "/setup", url.Values{"token": {e.d.SetupToken()}, "email": {"owner" + string(rune('a'+i)) + "@example.com"},
					"password": {setupPW}, "password2": {setupPW}, "org_name": {"Acme"}, "csrf": {m[1]}})
				codes[i] = res.StatusCode
			}()
		}
		wg.Wait()
		ok := 0
		for _, c := range codes {
			if c == 200 {
				ok++
			}
		}
		if ok != 1 || e.userCount() != 1 {
			t.Fatalf("statuses %v, users %d: want exactly one admin", codes, e.userCount())
		}
	})
}
