package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/oidc"
	"github.com/paymux/paymux/internal/oidc/oidctest"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

type sso struct {
	*env
	idp *oidctest.IdP
}

// newSSO starts the dashboard with a fake IdP; mod adjusts the SSO options.
func newSSO(t *testing.T, db *store.DB, mod func(*Options)) *sso {
	t.Helper()
	idp := oidctest.New(t)
	client := oidc.New(oidc.Config{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		RedirectURL: "http://localhost:8080/dashboard/login/oidc/callback", AllowInsecure: true})
	e := newEnv(t, db, func(o *Options) {
		o.OIDC, o.OIDCAllowedDomains = client, []string{"example.com"}
		if mod != nil {
			mod(o)
		}
	})
	return &sso{env: e, idp: idp}
}

// begin starts a sign-in as the given IdP user and returns the callback path
// (on the test server) the IdP sends the browser to.
func (s *sso) begin(c *http.Client, claims map[string]any) string {
	s.t.Helper()
	resp, _ := s.get(c, "/dashboard/login/oidc")
	if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), s.idp.Issuer+"/authorize?") {
		s.t.Fatalf("start: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	back, err := url.Parse(s.idp.Authorize(s.t, resp.Header.Get("Location"), claims))
	if err != nil {
		s.t.Fatal(err)
	}
	return back.Path + "?" + back.RawQuery
}

func ok(email string) map[string]any {
	return map[string]any{"sub": "sub-" + email, "email": email, "email_verified": true}
}

func TestSSOSignsInAnExistingUserWithoutTOTP(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		id, _ := s.seedUser("alice@example.com", "operator", true) // enrolled in TOTP, which SSO does not ask for
		s.allowLink(s.roleClient("admin"), id)                     // an admin lets the password account use SSO
		c := s.client()
		resp, body := s.get(c, s.begin(c, ok("Alice@Example.com")))
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("callback: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		if r, _ := s.get(c, "/dashboard/payments"); r.StatusCode != 200 {
			t.Errorf("signed in, but payments: %d", r.StatusCode)
		}
		if u, _ := db.UserByID(ctx, id); u.Role != "operator" {
			t.Errorf("role changed to %s", u.Role)
		}
		if auditCount(t, db, "user.sso_login") != 1 {
			t.Error("sign-in not audited")
		}
		// The login page offers SSO; password sign-in still goes through TOTP.
		if _, page := s.get(s.client(), "/dashboard/login"); !strings.Contains(page, "/dashboard/login/oidc") {
			t.Error("no SSO button on the login page")
		}
		pc := s.client()
		if r, _ := s.passwordStep(pc, "alice@example.com", goodPW); r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/login/totp") || sessionCookie(r) != nil {
			t.Errorf("password login must still require TOTP: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
	})
}

func TestSSOCreatesUsersOnlyForAllowedDomains(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		c := s.client()
		resp, _ := s.get(c, s.begin(c, ok("new.hire@example.com")))
		if resp.StatusCode != 303 || sessionCookie(resp) == nil {
			t.Fatalf("provisioning sign-in: %d", resp.StatusCode)
		}
		u, err := db.UserByEmail(ctx, "new.hire@example.com")
		if err != nil || u.Role != "viewer" || u.Disabled || u.TOTPSecretEnc != nil {
			t.Fatalf("created user %+v %v", u, err)
		}
		if auditAfter(t, db, "user.create") == "" || !strings.Contains(auditAfter(t, db, "user.create"), "sso") {
			t.Error("creation not audited as coming from SSO")
		}
		// The new user has no password: no password can ever work for it.
		for _, pw := range []string{goodPW, "!sso", "", "password"} {
			if r, body := s.passwordStep(s.client(), "new.hire@example.com", pw); r.StatusCode != 200 || !strings.Contains(body, "Invalid email or password") {
				t.Errorf("password %q worked for an SSO-created user: %d", pw, r.StatusCode)
			}
		}
		// The second sign-in finds the same user.
		c2 := s.client()
		if resp, _ := s.get(c2, s.begin(c2, ok("new.hire@example.com"))); resp.StatusCode != 303 {
			t.Errorf("second sign-in: %d", resp.StatusCode)
		}
		if n, _ := db.CountUsers(ctx); n != 1 {
			t.Errorf("users = %d", n)
		}

		for name, claims := range map[string]map[string]any{
			"other domain":     ok("mallory@evil.example.org"),
			"look-alike":       ok("mallory@example.com.evil.org"),
			"sub-domain":       ok("mallory@staff.example.com"),
			"unverified email": {"sub": "x", "email": "carol@example.com", "email_verified": false},
			"no verified flag": {"sub": "x", "email": "dave@example.com"},
			"no email":         {"sub": "x", "email_verified": true},
		} {
			c := s.client()
			resp, body := s.get(c, s.begin(c, claims))
			if resp.StatusCode == 303 || sessionCookie(resp) != nil || !strings.Contains(body, "Single sign-on failed") {
				t.Errorf("%s: %d", name, resp.StatusCode)
			}
		}
		if n, _ := db.CountUsers(ctx); n != 1 {
			t.Errorf("a refused sign-in created a user: %d users", n)
		}
	})
}

func TestSSODefaultRoleAndDisabledUsers(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, func(o *Options) { o.OIDCDefaultRole = "auditor" })
		_, _ = db.SaveRole(ctx, "auditor", []string{"payment.read", "audit.read"}, s.clk.Now())
		c := s.client()
		if resp, _ := s.get(c, s.begin(c, ok("aud@example.com"))); resp.StatusCode != 303 {
			t.Fatalf("sign-in: %d", resp.StatusCode)
		}
		if u, _ := db.UserByEmail(ctx, "aud@example.com"); u.Role != "auditor" {
			t.Errorf("default role %q", u.Role)
		}
		if r, _ := s.get(c, "/dashboard/audit"); r.StatusCode != 200 {
			t.Errorf("custom role through SSO: %d", r.StatusCode)
		}

		// A disabled user cannot sign in this way either.
		id, _ := s.seedUser("off@example.com", "viewer", true)
		_ = db.SetUserDisabled(ctx, id, true)
		c = s.client()
		if resp, _ := s.get(c, s.begin(c, ok("off@example.com"))); resp.StatusCode == 303 || sessionCookie(resp) != nil {
			t.Errorf("disabled user signed in: %d", resp.StatusCode)
		}
	})
	for name, role := range map[string]string{"admin": "admin", "missing": "nonesuch"} {
		storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
			s := newSSO(t, db, func(o *Options) { o.OIDCDefaultRole = role })
			c := s.client()
			if resp, _ := s.get(c, s.begin(c, ok("x@example.com"))); resp.StatusCode == 303 {
				t.Errorf("%s default role: a user was signed in", name)
			}
			if n, _ := db.CountUsers(ctx); n != 0 {
				t.Errorf("%s default role created %d users", name, n)
			}
		})
	}
}

func TestSSORejectsForgedAndReplayedCallbacks(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		id, _ := s.seedUser("alice@example.com", "viewer", true)
		s.allowLink(s.roleClient("admin"), id)
		fails := func(name string, c *http.Client, path string, want int) {
			t.Helper()
			resp, _ := s.get(c, path)
			if resp.StatusCode != want || sessionCookie(resp) != nil {
				t.Errorf("%s: %d (want %d) session=%v", name, resp.StatusCode, want, sessionCookie(resp) != nil)
			}
		}

		// state tampered
		c := s.client()
		path := s.begin(c, ok("alice@example.com"))
		fails("wrong state", c, strings.Replace(path, "state=", "state=x", 1), 400)

		// no cookie: the callback of someone else's sign-in
		c = s.client()
		path = s.begin(c, ok("alice@example.com"))
		fails("no sign-in in progress", s.client(), path, 400)

		// replay: the callback works once, the second use finds no sign-in in progress
		c = s.client()
		path = s.begin(c, ok("alice@example.com"))
		if resp, _ := s.get(c, path); resp.StatusCode != 303 {
			t.Fatalf("first use: %d", resp.StatusCode)
		}
		fails("replayed callback", c, path, 400)

		// a callback without a code, and an IdP error
		c = s.client()
		path = s.begin(c, ok("alice@example.com"))
		fails("error from the IdP", c, strings.Split(path, "?")[0]+"?error=access_denied&state="+url.QueryEscape(stateOf(path)), 401)

		// the IdP signs the token with a key it did not publish
		forged, _ := rsa.GenerateKey(rand.Reader, 2048)
		s.idp.SignWith = forged
		c = s.client()
		fails("forged signature", c, s.begin(c, ok("alice@example.com")), 401)
		s.idp.SignWith = nil

		// a token for another nonce (a replayed token), another audience
		s.idp.Mutate = func(cl map[string]any) { cl["nonce"] = "someone-elses" }
		c = s.client()
		fails("wrong nonce", c, s.begin(c, ok("alice@example.com")), 401)
		s.idp.Mutate = func(cl map[string]any) { cl["aud"] = "another-client" }
		c = s.client()
		fails("wrong audience", c, s.begin(c, ok("alice@example.com")), 401)
		s.idp.Mutate = nil

		// a tampered cookie
		c = s.client()
		path = s.begin(c, ok("alice@example.com"))
		u, _ := url.Parse(s.srv.URL + "/dashboard/login/oidc/callback")
		for _, ck := range c.Jar.Cookies(u) {
			if ck.Name == cookieOIDC {
				ck.Value = "x" + ck.Value
				c.Jar.SetCookies(u, []*http.Cookie{{Name: ck.Name, Value: ck.Value, Path: "/dashboard"}})
			}
		}
		fails("tampered sign-in cookie", c, path, 400)

		// after all that, a clean sign-in still works
		c = s.client()
		if resp, _ := s.get(c, s.begin(c, ok("alice@example.com"))); resp.StatusCode != 303 || sessionCookie(resp) == nil {
			t.Errorf("clean sign-in: %d", resp.StatusCode)
		}
	})
}

func stateOf(path string) string {
	u, _ := url.Parse(path)
	return u.Query().Get("state")
}

func TestSSOOffByDefault(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for _, p := range []string{"/dashboard/login/oidc", "/dashboard/login/oidc/callback?code=x&state=y"} {
			if r, _ := e.get(e.client(), p); r.StatusCode != 404 {
				t.Errorf("%s: %d", p, r.StatusCode)
			}
		}
		if _, page := e.get(e.client(), "/dashboard/login"); strings.Contains(page, "single sign-on") {
			t.Error("SSO button shown without SSO")
		}
	})
}
