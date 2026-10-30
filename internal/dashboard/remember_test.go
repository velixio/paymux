package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const day = 24 * time.Hour

func (e *env) setOrg(key, raw string) {
	e.t.Helper()
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, json.RawMessage(raw)); err != nil {
		e.t.Fatal(err)
	}
}

// loginAs runs the whole sign-in (password, then the authenticator code) and
// returns the response that created the session.
func (e *env) loginAs(c *http.Client, email, secret string, remember bool) *http.Response {
	e.t.Helper()
	_, page := e.get(c, "/dashboard/login")
	form := url.Values{"email": {email}, "password": {goodPW}, "csrf": {csrfOf(e.t, page)}}
	if remember {
		form.Set("remember", "1")
	}
	resp, body := e.post(c, "/dashboard/login", form)
	if resp.StatusCode != 303 || !strings.HasSuffix(resp.Header.Get("Location"), "/dashboard/login/totp") {
		e.t.Fatalf("password step: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	_, page = e.get(c, "/dashboard/login/totp")
	resp, body = e.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrfOf(e.t, page)}})
	if resp.StatusCode != 303 || sessionCookie(resp) == nil {
		e.t.Fatalf("totp step: %d\n%s", resp.StatusCode, body)
	}
	return resp
}

func (e *env) sessionOf(resp *http.Response) store.SessionRow {
	e.t.Helper()
	ck := sessionCookie(resp)
	if ck == nil {
		e.t.Fatal("no session cookie")
	}
	s, _, err := e.db.SessionByTokenHash(ctx, auth.HashToken(ck.Value))
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// live says whether the client's session still works.
func (e *env) live(c *http.Client) bool {
	e.t.Helper()
	r, _ := e.get(c, "/dashboard/payments")
	return r.StatusCode == 200
}

func TestRememberMeIsOffByDefaultAndTodaysLimitsApply(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		resp := e.loginAs(c, "alice@example.com", secret, false)
		ck := sessionCookie(resp)
		if ck.MaxAge != 0 || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
			t.Errorf("a session that was not remembered gets a session cookie: %+v", ck)
		}
		s := e.sessionOf(resp)
		if s.Remember || !s.ExpiresAt.Equal(s.CreatedAt.Add(7*day)) || !s.AuthenticatedAt.Equal(s.CreatedAt) {
			t.Errorf("row %+v", s)
		}
		if !e.live(c) {
			t.Fatal("not signed in")
		}
		e.clk.Add(13 * time.Hour) // idle for longer than 12 hours
		if e.live(c) {
			t.Error("a session idle for 13 hours is still alive")
		}
	})
}

func TestTheAbsoluteLimitOfASessionThatWasNotRemembered(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setOrg("session_idle_hours", "720") // activity cannot be what ends it
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		e.loginAs(c, "alice@example.com", secret, false)
		for range 6 { // active every day for six days
			e.clk.Add(day)
			if !e.live(c) {
				t.Fatal("ended early")
			}
		}
		e.clk.Add(2 * day) // day 8
		if e.live(c) {
			t.Error("a session older than 7 days is alive")
		}
	})
}

func TestRememberMeLastsThirtyDays(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		for _, tc := range []struct {
			after time.Duration
			alive bool
		}{{8 * day, true}, {29 * day, true}, {31 * day, false}} {
			c := e.client()
			resp := e.loginAs(c, "alice@example.com", secret, true)
			ck := sessionCookie(resp)
			if ck.MaxAge != 30*24*3600 || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie %+v", ck)
			}
			s := e.sessionOf(resp)
			if !s.Remember || !s.ExpiresAt.Equal(s.CreatedAt.Add(30*day)) {
				t.Fatalf("row %+v", s)
			}
			e.clk.Add(tc.after)
			if got := e.live(c); got != tc.alive {
				t.Errorf("after %v: alive = %v", tc.after, got)
			}
			e.clk.Add(time.Minute) // the next login needs a fresh authenticator code
		}
	})
}

// Staying active does not stretch the 30 days: they are counted from the sign-in.
func TestRememberMeNeverOutlivesThirtyDaysFromSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		e.loginAs(c, "alice@example.com", secret, true)
		for i := 1; i <= 3; i++ { // used every 9 days
			e.clk.Add(9 * day)
			if !e.live(c) {
				t.Fatalf("ended at day %d", 9*i)
			}
		}
		e.clk.Add(4 * day) // day 31, only 4 days since the last request
		if e.live(c) {
			t.Error("a remembered session outlived 30 days from sign-in")
		}
	})
}

func TestTheRememberChoiceCannotBeChangedAfterThePasswordStep(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		_, page := e.get(c, "/dashboard/login")
		e.post(c, "/dashboard/login", url.Values{"email": {"alice@example.com"}, "password": {goodPW}, "csrf": {csrfOf(t, page)}})
		_, page = e.get(c, "/dashboard/login/totp")
		// The second step asks for a longer life; it is not listened to.
		resp, _ := e.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrfOf(t, page)}, "remember": {"1"}})
		if s := e.sessionOf(resp); s.Remember || sessionCookie(resp).MaxAge != 0 {
			t.Errorf("the second step set remember: %+v", s)
		}
	})
}

func TestRememberMeSurvivesFirstTimeTwoFactorEnrolment(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, _ := e.seedUser("newbie@example.com", "viewer", false)
		c := e.client()
		_, page := e.get(c, "/dashboard/login")
		e.post(c, "/dashboard/login", url.Values{"email": {"newbie@example.com"}, "password": {goodPW}, "csrf": {csrfOf(t, page)}, "remember": {"1"}})
		code, _ := e.users().IssueEnrolmentToken(ctx, id)
		_, page = e.get(c, "/dashboard/login/totp")
		_, body := e.post(c, "/dashboard/login/totp", url.Values{"enrol_code": {code}, "csrf": {csrfOf(t, page)}})
		m := regexp.MustCompile(`<code id="totp-secret">([A-Z2-7]+)</code>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no secret:\n%s", body)
		}
		resp, _ := e.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(m[1], e.clk.Now(), 0)}, "csrf": {csrfOf(t, body)}})
		if sessionCookie(resp) == nil {
			t.Fatalf("not signed in: %d", resp.StatusCode)
		}
		if s := e.sessionOf(resp); !s.Remember || !s.ExpiresAt.Equal(s.CreatedAt.Add(30*day)) {
			t.Errorf("enrolment lost the choice: %+v", s)
		}
	})
}

func TestRememberMeThroughSingleSignOn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		for _, remember := range []bool{false, true} {
			c := s.client()
			start := "/dashboard/login/oidc"
			if remember {
				start += "?remember=1"
			}
			resp, _ := s.get(c, start)
			back, err := url.Parse(s.idp.Authorize(t, resp.Header.Get("Location"), ok("sso.person@example.com")))
			if err != nil {
				t.Fatal(err)
			}
			resp, _ = s.get(c, back.Path+"?"+back.RawQuery)
			row := s.sessionOf(resp)
			if row.Remember != remember {
				t.Errorf("remember=%v: row %+v", remember, row)
			}
			if remember && sessionCookie(resp).MaxAge != 30*24*3600 {
				t.Errorf("cookie %+v", sessionCookie(resp))
			}
			if !remember && sessionCookie(resp).MaxAge != 0 {
				t.Errorf("cookie %+v", sessionCookie(resp))
			}
		}
	})
}

func TestTheRememberSettingAndTheCheckbox(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		_, page := e.get(e.client(), "/dashboard/login")
		// An accessible checkbox: a label that holds it, off by default, with the number of days.
		box := regexp.MustCompile(`(?s)<label[^>]*>\s*<input[^>]*type="checkbox"[^>]*name="remember"[^>]*>[^<]*Remember me for 30 days`).FindString(page)
		if box == "" || strings.Contains(box, "checked") {
			t.Fatalf("no unchecked, labelled checkbox on the login page:\n%s", page)
		}

		e.setOrg("session_remember_days", "10")
		_, page = e.get(e.client(), "/dashboard/login")
		if !strings.Contains(page, "Remember me for 10 days") {
			t.Error("the label does not follow the setting")
		}
		c := e.client()
		resp := e.loginAs(c, "alice@example.com", secret, true)
		if s := e.sessionOf(resp); !s.ExpiresAt.Equal(s.CreatedAt.Add(10 * day)) {
			t.Errorf("10 days: %+v", s)
		}

		// 0 takes the checkbox away and ignores a form that sends it anyway.
		e.setOrg("session_remember_days", "0")
		_, page = e.get(e.client(), "/dashboard/login")
		if strings.Contains(page, `name="remember"`) {
			t.Error("the checkbox is shown although remembering is off")
		}
		e.clk.Add(time.Minute)
		resp = e.loginAs(e.client(), "alice@example.com", secret, true)
		if s := e.sessionOf(resp); s.Remember || sessionCookie(resp).MaxAge != 0 {
			t.Errorf("remembered although switched off: %+v", s)
		}
	})
}

func TestSessionLimitsFollowTheSettings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setOrg("session_idle_hours", "2")
		e.setOrg("session_max_days", "3")
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		resp := e.loginAs(c, "alice@example.com", secret, false)
		if s := e.sessionOf(resp); !s.ExpiresAt.Equal(s.CreatedAt.Add(3 * day)) {
			t.Errorf("max age %+v", s)
		}
		e.clk.Add(3 * time.Hour)
		if e.live(c) {
			t.Error("idle for 3 hours with a 2 hour limit")
		}
	})
}

// The three durations are on the settings page with their value and where it comes from.
func TestSessionSettingsAreOnTheSettingsPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		admin := e.roleClient("admin")
		page := e.pageOK(admin, "/dashboard/settings")
		has(t, page, "session_remember_days", "session_idle_hours", "session_max_days", "Sign-in sessions")
	})
}
