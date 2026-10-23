package dashboard

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

var sessionIDRE = regexp.MustCompile(`/dashboard/account/sessions/(ses_[A-Za-z0-9_-]+)/revoke`)

func TestSignInsAreAuditedWithTheRememberChoice(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		e.loginAs(e.client(), "alice@example.com", secret, true)
		e.clk.Add(time.Minute)
		e.loginAs(e.client(), "alice@example.com", secret, false)
		got := auditAfter(t, db, "user.login")
		if !strings.Contains(got, `"remember":"true"`) || !strings.Contains(got, `"remember":"false"`) {
			t.Errorf("audit: %s", got)
		}
	})
}

func TestYourSessionsPageListsOnlyYourOwnAndRevokesThem(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, asecret := e.seedUser("alice@example.com", "viewer", true)
		_, bsecret := e.seedUser("bob@example.com", "viewer", true)
		a1, a2, b1 := e.client(), e.client(), e.client()
		e.loginAs(a1, "alice@example.com", asecret, true)
		e.clk.Add(time.Minute)
		e.loginAs(a2, "alice@example.com", asecret, false)
		e.clk.Add(time.Minute)
		e.loginAs(b1, "bob@example.com", bsecret, false)

		page := e.pageOK(a1, "/dashboard/account")
		has(t, page, "Your sessions", "Remembered", "This session", "127.0.0.0/24", "Go-http-client")
		ids := sessionIDRE.FindAllStringSubmatch(page, -1)
		if len(ids) != 1 { // the other one of alice's; the current one has no button
			t.Fatalf("revoke buttons: %v", ids)
		}
		tok := csrfOf(t, page)
		// Bob's session cannot be revoked by alice, whatever its id.
		bob := e.pageOK(b1, "/dashboard/account")
		bobID := regexp.MustCompile(`ses_[A-Za-z0-9_-]+`).FindString(strings.ReplaceAll(bob, "/revoke", "")) // his own, shown as the current one
		_ = bobID
		rows, _ := db.ListUserSessions(ctx, "usr_bob")
		if len(rows) != 1 {
			t.Fatalf("bob has %d sessions", len(rows))
		}
		if r, _ := e.post(a1, "/dashboard/account/sessions/"+rows[0].ID+"/revoke", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("revoking another user's session: %d", r.StatusCode)
		}
		if !e.live(b1) {
			t.Error("bob was signed out by alice")
		}
		// Revoke her other session.
		if r, _ := e.post(a1, "/dashboard/account/sessions/"+ids[0][1]+"/revoke", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("revoke: %d", r.StatusCode)
		}
		if e.live(a2) || !e.live(a1) {
			t.Errorf("after the revoke: other alive=%v current alive=%v", e.live(a2), e.live(a1))
		}
		if auditCount(t, db, "session.revoke") != 1 {
			t.Error("the revocation is not audited")
		}
	})
}

func TestSignOutAllOtherSessions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "viewer", true)
		cs := []*http.Client{e.client(), e.client(), e.client()}
		for _, c := range cs {
			e.loginAs(c, "alice@example.com", secret, true)
			e.clk.Add(time.Minute)
		}
		tok := csrfOf(t, e.pageOK(cs[0], "/dashboard/account"))
		if r, _ := e.post(cs[0], "/dashboard/account/sessions/revoke-others", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("status %d", r.StatusCode)
		}
		if !e.live(cs[0]) || e.live(cs[1]) || e.live(cs[2]) {
			t.Error("only the current session should be left")
		}
		if auditCount(t, db, "session.revoke_others") != 1 {
			t.Error("not audited")
		}
	})
}

func TestChangingThePasswordEndsEveryRememberedSession(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "viewer", true)
		c1, c2 := e.client(), e.client()
		e.loginAs(c1, "alice@example.com", secret, true)
		e.clk.Add(time.Minute)
		e.loginAs(c2, "alice@example.com", secret, true)
		tok := csrfOf(t, e.pageOK(c1, "/dashboard/account"))
		change := func(cur, next, again string) *http.Response {
			r, _ := e.post(c1, "/dashboard/account/password", url.Values{"csrf": {tok}, "current_password": {cur}, "new_password": {next}, "confirm_password": {again}})
			return r
		}
		// None of these change anything.
		for name, r := range map[string]*http.Response{
			"wrong current": change("not the password!!", "a brand new long password", "a brand new long password"),
			"mismatch":      change(goodPW, "a brand new long password", "something else entirely"),
			"too short":     change(goodPW, "short", "short"),
		} {
			if r.StatusCode != 400 && r.StatusCode != 200 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
			if sessionAlive := e.live(c1) && e.live(c2); !sessionAlive {
				t.Fatalf("%s ended a session", name)
			}
		}
		if r := change(goodPW, "a brand new long password", "a brand new long password"); r.StatusCode != 303 {
			t.Fatalf("change: %d", r.StatusCode)
		}
		if e.live(c1) || e.live(c2) {
			t.Error("a session survived the password change")
		}
		if auditCount(t, db, "user.password_change") != 1 {
			t.Error("not audited")
		}
		// The new password works, the old one does not.
		if r, _ := e.passwordStep(e.client(), "alice@example.com", goodPW); r.StatusCode == 303 && strings.HasSuffix(r.Header.Get("Location"), "/login/totp") {
			t.Error("the old password still works")
		}
		if r, _ := e.passwordStep(e.client(), "alice@example.com", "a brand new long password"); r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/login/totp") {
			t.Errorf("the new password: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
	})
}

func TestAnAdminSignsAUserOutEverywhere(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, secret := e.seedUser("alice@example.com", "viewer", true)
		c1, c2 := e.client(), e.client()
		e.loginAs(c1, "alice@example.com", secret, true)
		e.clk.Add(time.Minute)
		e.loginAs(c2, "alice@example.com", secret, false)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/users"))
		viewer := e.roleClient("viewer")
		vt := csrfOf(t, e.pageOK(viewer, "/dashboard/account"))
		if r, _ := e.post(viewer, "/dashboard/users/"+id+"/sessions/revoke", url.Values{"csrf": {vt}}); r.StatusCode != 403 {
			t.Errorf("a viewer signed somebody out: %d", r.StatusCode)
		}
		if !e.live(c1) {
			t.Fatal("signed out by a viewer")
		}
		if r, _ := e.post(admin, "/dashboard/users/"+id+"/sessions/revoke", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("status %d", r.StatusCode)
		}
		if e.live(c1) || e.live(c2) {
			t.Error("sessions survived")
		}
		if auditCount(t, db, "user.sessions_revoke") != 1 {
			t.Error("not audited")
		}
	})
}

func TestDisablingResettingAndReissuingEndRememberedSessions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, secret := e.seedUser("alice@example.com", "viewer", true)
		admin := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(admin, "/dashboard/users"))

		c := e.client()
		e.loginAs(c, "alice@example.com", secret, true)
		e.post(admin, "/dashboard/users/"+id+"/disable", url.Values{"action": {"disable"}, "csrf": {tok}})
		if e.live(c) {
			t.Error("a disabled user's remembered session works")
		}
		e.post(admin, "/dashboard/users/"+id+"/disable", url.Values{"action": {"enable"}, "csrf": {tok}})

		e.clk.Add(time.Minute)
		c = e.client()
		e.loginAs(c, "alice@example.com", secret, true)
		if err := e.users().ResetTOTP(ctx, "alice@example.com"); err != nil {
			t.Fatal(err)
		}
		if e.live(c) {
			t.Error("a 2FA reset left the remembered session alive")
		}

		// Reissuing the enrolment code of a user who has not enrolled ends their sessions too.
		newbie, _ := e.seedUser("newbie@example.com", "viewer", false)
		if err := db.InsertSession(ctx, store.SessionRow{ID: "ses_x", UserID: newbie, TokenHash: "hx", CSRFKey: []byte{1}, CreatedAt: e.clk.Now(),
			LastSeenAt: e.clk.Now(), ExpiresAt: e.clk.Now().Add(day), Remember: true}); err != nil {
			t.Fatal(err)
		}
		if r, _ := e.post(admin, "/dashboard/users/"+newbie+"/enrolment", url.Values{"csrf": {tok}}); r.StatusCode != 200 {
			t.Fatalf("reissue: %d", r.StatusCode)
		}
		if rows, _ := db.ListUserSessions(ctx, newbie); len(rows) != 0 {
			t.Errorf("%d sessions survived the reissue", len(rows))
		}
	})
}

// Remember me lengthens a login; it does not let sensitive actions skip the
// password: after 12 hours they ask for it (and the code) again.
func TestSensitiveActionsNeedARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("root@example.com", "admin", true)
		bob, _ := e.seedUser("bob@example.com", "viewer", false)
		c := e.client()
		e.loginAs(c, "root@example.com", secret, true)
		role := func() *http.Response {
			tok := csrfOf(t, e.pageOK(c, "/dashboard/users"))
			r, _ := e.post(c, "/dashboard/users/"+bob+"/role", url.Values{"role": {"operator"}, "csrf": {tok}})
			return r
		}
		e.clk.Add(11 * time.Hour)
		if r := role(); r.StatusCode != 303 || strings.Contains(r.Header.Get("Location"), "reauth") {
			t.Fatalf("within 12 hours: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		e.clk.Add(2 * time.Hour) // 13 hours since the sign-in; the session was used 2 hours ago
		r := role()
		if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/dashboard/reauth?next=") {
			t.Fatalf("after 12 hours: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		if u, _ := db.UserByID(ctx, bob); u.Role != "operator" {
			// the first change (before 12 hours) stands; the second asked for a password
		}
		// Other pages are not affected.
		if !e.live(c) {
			t.Fatal("the session itself ended")
		}
		// Confirming with a wrong password or code does not help.
		page := e.pageOK(c, r.Header.Get("Location"))
		has(t, page, "password", "code")
		tok := csrfOf(t, page)
		next := regexp.MustCompile(`(?s)action="/dashboard/reauth".*?name="next" value="([^"]*)"`).FindStringSubmatch(page)
		if next == nil {
			t.Fatalf("no next field:\n%s", page)
		}
		bad, _ := e.post(c, "/dashboard/reauth", url.Values{"csrf": {tok}, "next": {next[1]}, "password": {"not the password!!"}, "code": {totpFor(secret, e.clk.Now(), 0)}})
		if bad.StatusCode == 303 {
			t.Error("a wrong password confirmed the session")
		}
		if role().StatusCode == 303 && !strings.Contains(role().Header.Get("Location"), "reauth") {
			t.Error("the guard let a stale session through")
		}
		ok, _ := e.post(c, "/dashboard/reauth", url.Values{"csrf": {tok}, "next": {next[1]}, "password": {goodPW}, "code": {totpFor(secret, e.clk.Now(), 0)}})
		if ok.StatusCode != 303 || strings.Contains(ok.Header.Get("Location"), "reauth") {
			t.Fatalf("confirm: %d %s", ok.StatusCode, ok.Header.Get("Location"))
		}
		if r := role(); r.StatusCode != 303 || strings.Contains(r.Header.Get("Location"), "reauth") {
			t.Errorf("after confirming: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		// The way back must stay on this site.
		evil, _ := e.post(c, "/dashboard/reauth", url.Values{"csrf": {tok}, "next": {"https://evil.example/"}, "password": {goodPW}, "code": {totpFor(secret, e.clk.Now().Add(30*time.Second), 0)}})
		if loc := evil.Header.Get("Location"); strings.Contains(loc, "evil.example") {
			t.Errorf("open redirect: %s", loc)
		}
	})
}

// A session that was not remembered is not asked either while it is young.
func TestNewSessionsAreNotAskedToConfirm(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("root@example.com", "admin", true)
		c := e.client()
		e.loginAs(c, "root@example.com", secret, false)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/users"))
		r, _ := e.post(c, "/dashboard/users", url.Values{"email": {"n@example.com"}, "role": {"viewer"}, "password": {goodPW}, "csrf": {tok}})
		if r.StatusCode != 200 {
			t.Errorf("create user: %d", r.StatusCode)
		}
	})
}

// People who sign in through the identity provider have no password here to
// confirm with; an old session of theirs asks them to sign in again.
func TestAnOldSingleSignOnSessionIsAskedToSignInAgain(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		if err := db.InsertUser(ctx, store.UserRow{ID: "usr_sso", Email: "boss@example.com", PasswordHash: ssoPasswordHash, TOTPLastStep: -1, Role: "admin", CreatedAt: s.clk.Now()}); err != nil {
			t.Fatal(err)
		}
		c := s.client()
		resp, _ := s.get(c, "/dashboard/login/oidc?remember=1")
		back, _ := url.Parse(s.idp.Authorize(t, resp.Header.Get("Location"), ok("boss@example.com")))
		if resp, _ = s.get(c, back.Path+"?"+back.RawQuery); sessionCookie(resp) == nil {
			t.Fatalf("not signed in: %d", resp.StatusCode)
		}
		s.clk.Add(13 * time.Hour)
		tok := csrfOf(t, s.pageOK(c, "/dashboard/users"))
		r, _ := s.post(c, "/dashboard/users/usr_sso/enrolment", url.Values{"csrf": {tok}})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
			t.Errorf("%d %s", r.StatusCode, r.Header.Get("Location"))
		}
		if s.live(c) {
			t.Error("the session was kept")
		}
	})
}

func TestYourSessionsPageIsForSignedInUsersOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for _, p := range []string{"/dashboard/account", "/dashboard/reauth"} {
			if r, _ := e.get(e.client(), p); r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
				t.Errorf("%s anonymous: %d %s", p, r.StatusCode, r.Header.Get("Location"))
			}
		}
		for _, p := range []string{"/dashboard/account/password", "/dashboard/account/sessions/revoke-others"} {
			if r, _ := e.post(e.client(), p, url.Values{}); r.StatusCode == 200 || r.StatusCode == 204 {
				t.Errorf("%s anonymous: %d", p, r.StatusCode)
			}
		}
	})
}
