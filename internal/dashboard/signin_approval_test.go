package dashboard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const (
	testPIN       = "482913"
	waitingPath   = "/dashboard/login/waiting"
	statusPath    = "/dashboard/login/waiting/status"
	waitingWords  = "Waiting for external auth"
	endedWords    = "This sign-in request ended."
	signinToken   = "123456:ABC_def-789"
	signinCookies = "pm_signin_wait"
)

// signinChannel turns on the live Telegram channel.
func (e *env) signinChannel() {
	e.t.Helper()
	org, err := e.d.orgID(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := outbound.NotificationConfig{TelegramChatID: "-100123", TelegramEnabled: true}
	if err := e.out.SaveNotificationConfig(ctx, org, "live", cfg, signinToken); err != nil {
		e.t.Fatal(err)
	}
}

// linkTelegram connects the user to Telegram account 42, the fake's private chat.
func (e *env) linkTelegram(userID string) {
	e.t.Helper()
	org, err := e.d.orgID(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.db.AddTelegramLink(ctx, org, userID, "code_"+userID, e.clk.Now().Add(time.Minute)); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.UseTelegramLink(ctx, org, "code_"+userID, 42, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// setTap links the user and turns tap approval on.
func (e *env) setTap(userID string) {
	e.t.Helper()
	e.linkTelegram(userID)
	if err := e.db.SetUserSigninTap(ctx, userID, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// setPIN links the user and turns PIN approval on with testPIN.
func (e *env) setPIN(userID string) {
	e.t.Helper()
	e.linkTelegram(userID)
	enc, err := e.box.Seal([]byte(testPIN), secrets.AADSigninPIN(userID))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.db.SetUserSigninPIN(ctx, userID, enc, e.clk.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// pinUser seeds an enrolled user who has a PIN, with the channel on.
func (e *env) pinUser(email string) (id, secret string) {
	e.t.Helper()
	e.signinChannel()
	id, secret = e.seedUser(email, "admin", true)
	e.setPIN(id)
	return id, secret
}

// signIn runs the password and code steps and returns the code step's answer.
func (e *env) signIn(c *http.Client, email, secret string, remember bool) (*http.Response, string) {
	e.t.Helper()
	_, page := e.get(c, "/dashboard/login")
	form := url.Values{"email": {email}, "password": {goodPW}, "csrf": {csrfOf(e.t, page)}}
	if remember {
		form.Set("remember", "1")
	}
	if resp, body := e.post(c, "/dashboard/login", form); resp.StatusCode != 303 {
		e.t.Fatalf("password step: %d\n%s", resp.StatusCode, body)
	}
	_, page = e.get(c, "/dashboard/login/totp")
	return e.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrfOf(e.t, page)}})
}

// toWaiting signs in as far as the waiting page and returns the sent message id.
func (e *env) toWaiting(c *http.Client, email, secret string, remember bool) int64 {
	e.t.Helper()
	resp, body := e.signIn(c, email, secret, remember)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != waitingPath || sessionCookie(resp) != nil {
		e.t.Fatalf("code step: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	e.signinTick()
	sent := e.tg.Sent()
	if len(sent) == 0 {
		e.t.Fatal("no notification sent")
	}
	return sent[len(sent)-1].MessageID
}

func (e *env) signinTick() {
	e.t.Helper()
	e.clk.Add(2 * time.Second)
	if _, err := e.q.RunOnce(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// nextCode moves past the authenticator step the last code used.
func (e *env) nextCode() { e.clk.Add(31 * time.Second) }

func (e *env) sessions() int { return e.count(`SELECT COUNT(*) FROM sessions`) }

func (e *env) wantNoSession(resp *http.Response) {
	e.t.Helper()
	if sessionCookie(resp) != nil || e.sessions() != 0 {
		e.t.Fatalf("a session exists: cookie %v, %d rows", sessionCookie(resp), e.sessions())
	}
}

func TestNoSessionBeforeTheCorrectPIN(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, true)
		e.wantNoSession(&http.Response{})
		resp, body := e.get(e.c, waitingPath)
		e.wantNoSession(resp)
		if resp.StatusCode != 200 || !strings.Contains(body, waitingWords) {
			t.Fatalf("waiting: %d\n%s", resp.StatusCode, body)
		}
		if r, _ := e.get(e.c, "/dashboard/payments"); r.StatusCode != 303 {
			t.Fatalf("payments while waiting: %d", r.StatusCode)
		}
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		resp, body = e.get(e.c, waitingPath)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("after the PIN: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		if !e.sessionOf(resp).Remember {
			t.Error("the remember choice was lost")
		}
		if r, _ := e.get(e.c, "/dashboard/payments"); r.StatusCode != 200 {
			t.Errorf("signed in, but payments: %d", r.StatusCode)
		}
	})
}

func TestSSONeedsThePINToo(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		s.signinChannel()
		id, _ := s.seedUser("alice@example.com", "operator", true)
		s.allowLink(s.roleClient("admin"), id)
		sessionsBefore := s.sessions()
		s.setPIN(id)
		c := s.client()
		resp, body := s.get(c, s.begin(c, ok("Alice@Example.com")))
		if resp.StatusCode != 303 || resp.Header.Get("Location") != waitingPath || sessionCookie(resp) != nil || s.sessions() != sessionsBefore {
			t.Fatalf("callback: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		s.signinTick()
		sent := s.tg.Sent()
		s.tg.Reply(sent[len(sent)-1].MessageID, testPIN)
		s.signinTick()
		resp, _ = s.get(c, waitingPath)
		if resp.StatusCode != 303 || sessionCookie(resp) == nil {
			t.Fatalf("after the PIN: %d", resp.StatusCode)
		}
	})
}

func TestNoPINSignsInAsBefore(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("plain@example.com", "admin", true)
		resp, _ := e.signIn(e.c, "plain@example.com", secret, false)
		if resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("code step: %s", resp.Header.Get("Location"))
		}
		if n := e.count(`SELECT COUNT(*) FROM signin_approvals`); n != 0 {
			t.Fatalf("%d requests", n)
		}
	})
}

func TestDevSkipSkipsThePIN(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(o *Options) { o.DevSkip2FA = true })
		e.pinUser("pin@example.com")
		resp, _ := e.passwordStep(e.c, "pin@example.com", goodPW)
		if resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("password step: %s", resp.Header.Get("Location"))
		}
	})
}

func TestWaitingPageStatesWording(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		_, body := e.get(e.c, waitingPath)
		if !strings.Contains(body, waitingWords) || !strings.Contains(body, `data-signin-wait="`+statusPath+`"`) {
			t.Fatalf("pending page:\n%s", body)
		}

		for i := range 5 {
			e.tg.Reply(msg, fmt.Sprintf("11111%d", i))
		}
		e.signinTick()
		resp, body := e.get(e.c, waitingPath)
		if !strings.Contains(body, endedWords) || !strings.Contains(body, `href="/dashboard/login"`) || strings.Contains(body, waitingWords) {
			t.Fatalf("ended page:\n%s", body)
		}
		e.wantNoSession(resp)
		cleared := false
		for _, c := range resp.Cookies() {
			cleared = cleared || (c.Name == signinCookies && c.MaxAge < 0)
		}
		if !cleared {
			t.Error("the waiting cookie was not cleared")
		}
		if r, _ := e.get(e.c, waitingPath); r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/login" {
			t.Errorf("without the cookie: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
	})
}

func TestExpiryEndsTheWaitWithoutATick(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		e.toWaiting(e.c, "pin@example.com", secret, false)
		e.clk.Add(5 * time.Minute)
		resp, body := e.get(e.c, waitingPath)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("after expiry:\n%s", body)
		}
		e.wantNoSession(resp)
	})
}

func TestSignInResponsesNeverMentionTheChannel(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		s.signinChannel()
		id, secret := s.seedUser("pin@example.com", "admin", true)
		s.allowLink(s.roleClient("admin"), id)
		s.setPIN(id)
		c := s.client()
		check := func(step string, resp *http.Response, body string) {
			t.Helper()
			text := strings.ToLower(fmt.Sprint(resp.Header) + body)
			if strings.Contains(text, "telegram") {
				t.Errorf("%s mentions it:\n%s", step, body)
			}
		}
		_, page := s.get(c, "/dashboard/login")
		r, b := s.post(c, "/dashboard/login", url.Values{"email": {"pin@example.com"}, "password": {goodPW}, "csrf": {csrfOf(t, page)}})
		check("password", r, b)
		r, page = s.get(c, "/dashboard/login/totp")
		check("code page", r, page)
		r, b = s.post(c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, s.clk.Now(), 0)}, "csrf": {csrfOf(t, page)}})
		check("code", r, b)
		r, b = s.get(c, waitingPath)
		check("waiting", r, b)
		r, b = s.get(c, statusPath)
		check("status", r, b)
		s.signinTick()
		sent := s.tg.Sent()
		for i := range 5 {
			s.tg.Reply(sent[len(sent)-1].MessageID, fmt.Sprintf("11111%d", i))
		}
		s.signinTick()
		r, b = s.get(c, statusPath)
		check("status ended", r, b)
		r, b = s.get(c, waitingPath)
		check("ended", r, b)
		c2 := s.client()
		r, b = s.get(c2, s.begin(c2, ok("pin@example.com")))
		check("sso callback", r, b)
		r, b = s.get(c2, waitingPath)
		check("sso waiting", r, b)
		// Locked out: five more wrong make ten.
		s.signinTick()
		sent = s.tg.Sent()
		for i := range 5 {
			s.tg.Reply(sent[len(sent)-1].MessageID, fmt.Sprintf("22222%d", i))
		}
		s.signinTick()
		s.nextCode()
		r, b = s.env.signIn(s.client(), "pin@example.com", secret, false)
		if r.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("not locked out: %d", r.StatusCode)
		}
		check("locked out", r, b)
	})
}

func TestStatusEndpoint(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		if _, body := e.get(e.c, statusPath); body != "ready" {
			t.Fatalf("no cookie: %q", body)
		}
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		resp, body := e.get(e.c, statusPath)
		if body != "pending" || resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("pending: %d %q %v", resp.StatusCode, body, resp.Header)
		}
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		resp, body = e.get(e.c, statusPath)
		if body != "ready" {
			t.Fatalf("approved: %q", body)
		}
		e.wantNoSession(resp)
		if n := e.count(`SELECT COUNT(*) FROM signin_approvals WHERE status = 'approved'`); n != 1 {
			t.Fatalf("the status check changed the request: %d approved", n)
		}
	})
}

func TestAnotherBrowserCannotCollect(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		other := e.client()
		u, _ := url.Parse(e.srv.URL + "/dashboard/")
		other.Jar.SetCookies(u, []*http.Cookie{{Name: signinCookies, Value: "made-up", Path: "/dashboard"}})
		resp, _ := e.get(other, waitingPath)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/login" {
			t.Fatalf("other browser: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		e.wantNoSession(resp)
		if r, _ := e.get(e.c, waitingPath); sessionCookie(r) == nil {
			t.Fatal("the right browser could not collect")
		}
	})
}

func TestCollectingWorksOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		u, _ := url.Parse(e.srv.URL + "/dashboard/")
		var saved *http.Cookie
		for _, c := range e.c.Jar.Cookies(u) {
			if c.Name == signinCookies {
				saved = c
			}
		}
		if saved == nil {
			t.Fatal("no waiting cookie")
		}
		if r, _ := e.get(e.c, waitingPath); sessionCookie(r) == nil {
			t.Fatal("first collect failed")
		}
		replay := e.client()
		replay.Jar.SetCookies(u, []*http.Cookie{{Name: signinCookies, Value: saved.Value, Path: "/dashboard"}})
		resp, body := e.get(replay, waitingPath)
		if sessionCookie(resp) != nil || e.sessions() != 1 || !strings.Contains(body, endedWords) {
			t.Fatalf("replay: %d sessions\n%s", e.sessions(), body)
		}
	})
}

func TestDisabledUserCannotCollect(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		if err := db.SetUserDisabled(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		resp, body := e.get(e.c, waitingPath)
		e.wantNoSession(resp)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("page:\n%s", body)
		}
	})
}

func TestRemovedPINCannotCollect(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		if err := db.ClearUserSigninApproval(ctx, id, e.clk.Now()); err != nil {
			t.Fatal(err)
		}
		resp, _ := e.get(e.c, waitingPath)
		e.wantNoSession(resp)
	})
}

func TestSecondSignInReplacesTheFirst(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		first, second := e.client(), e.client()
		e.toWaiting(first, "pin@example.com", secret, false)
		e.nextCode()
		msg := e.toWaiting(second, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		resp, body := e.get(first, waitingPath)
		e.wantNoSession(resp)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("first browser:\n%s", body)
		}
		if r, _ := e.get(second, waitingPath); sessionCookie(r) == nil {
			t.Fatal("the second browser could not collect")
		}
	})
}

func TestApprovalTooLateToCollect(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		msg := e.toWaiting(e.c, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		e.clk.Add(3 * time.Minute)
		resp, body := e.get(e.c, waitingPath)
		e.wantNoSession(resp)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("page:\n%s", body)
		}
	})
}

func TestWrongPINsLockTheAccount(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		for round := range 2 {
			msg := e.toWaiting(e.client(), "pin@example.com", secret, false)
			for i := range 5 {
				e.tg.Reply(msg, fmt.Sprintf("1%d111%d", round, i))
			}
			e.signinTick()
			e.nextCode()
		}
		before := e.count(`SELECT COUNT(*) FROM signin_approvals`)
		resp, body := e.signIn(e.client(), "pin@example.com", secret, false)
		if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgLockedOut) || sessionCookie(resp) != nil {
			t.Fatalf("locked out: %d\n%s", resp.StatusCode, body)
		}
		if n := e.count(`SELECT COUNT(*) FROM signin_approvals`); n != before {
			t.Fatalf("a request was started while locked out: %d", n-before)
		}
		e.clk.Add(16 * time.Minute)
		e.toWaiting(e.client(), "pin@example.com", secret, false)
	})
}

func TestTOTPTicketIsSpentOnceWaiting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		_, page := e.get(e.c, "/dashboard/login")
		if r, _ := e.post(e.c, "/dashboard/login", url.Values{"email": {"pin@example.com"}, "password": {goodPW}, "csrf": {csrfOf(t, page)}}); r.StatusCode != 303 {
			t.Fatalf("password: %d", r.StatusCode)
		}
		u, _ := url.Parse(e.srv.URL + "/dashboard/")
		var ticket *http.Cookie
		for _, c := range e.c.Jar.Cookies(u) {
			if c.Name == cookieTicket {
				ticket = c
			}
		}
		_, page = e.get(e.c, "/dashboard/login/totp")
		csrf := csrfOf(t, page)
		if r, _ := e.post(e.c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrf}}); r.Header.Get("Location") != waitingPath {
			t.Fatalf("code: %s", r.Header.Get("Location"))
		}
		for _, c := range e.c.Jar.Cookies(u) {
			if c.Name == cookieTicket {
				t.Fatal("the ticket cookie is still set")
			}
		}
		e.nextCode()
		e.c.Jar.SetCookies(u, []*http.Cookie{{Name: cookieTicket, Value: ticket.Value, Path: "/dashboard"}})
		r, _ := e.post(e.c, "/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrf}})
		if r.Header.Get("Location") != "/dashboard/login" {
			t.Fatalf("replayed ticket: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		if n := e.count(`SELECT COUNT(*) FROM signin_approvals`); n != 1 {
			t.Fatalf("%d requests", n)
		}
	})
}

func TestASecondSignInCancelsAnUncollectedApproval(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.pinUser("pin@example.com")
		first, second := e.client(), e.client()
		msg := e.toWaiting(first, "pin@example.com", secret, false)
		e.tg.Reply(msg, testPIN)
		e.signinTick()
		e.nextCode()
		e.toWaiting(second, "pin@example.com", secret, false)
		resp, body := e.get(first, waitingPath)
		e.wantNoSession(resp)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("first browser:\n%s", body)
		}
	})
}

func TestTappingApproveSignsIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.signinChannel()
		id, secret := e.seedUser("tap@example.com", "admin", true)
		e.setTap(id)
		msg := e.toWaiting(e.c, "tap@example.com", secret, false)
		if sent := e.tg.Sent(); sent[len(sent)-1].ChatID != "42" || len(sent[len(sent)-1].Buttons) != 2 {
			t.Fatalf("notification %+v", sent[len(sent)-1])
		}
		resp, _ := e.get(e.c, waitingPath)
		e.wantNoSession(resp)
		e.tg.Tap(msg, "signin:approve")
		e.signinTick()
		resp, _ = e.get(e.c, waitingPath)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("not signed in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
	})
}

func TestTappingDenyEndsTheSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.signinChannel()
		id, secret := e.seedUser("tap@example.com", "admin", true)
		e.setTap(id)
		msg := e.toWaiting(e.c, "tap@example.com", secret, false)
		e.tg.Tap(msg, "signin:deny")
		e.signinTick()
		resp, body := e.get(e.c, waitingPath)
		e.wantNoSession(resp)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("page:\n%s", body)
		}
	})
}

func TestLinkedTelegramWithoutAMethodSignsInAsBefore(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.signinChannel()
		id, secret := e.seedUser("plain@example.com", "admin", true)
		e.linkTelegram(id)
		resp, _ := e.signIn(e.c, "plain@example.com", secret, false)
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/" || sessionCookie(resp) == nil {
			t.Fatalf("got %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
	})
}
