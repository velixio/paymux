package dashboard

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/outbound/telegramtest"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const (
	approvalRoute = "/dashboard/account/signin-approval"
	linkRoute     = "/dashboard/account/telegram/link"
	unlinkRoute   = "/dashboard/account/telegram/unlink"
)

// accountUser signs in a user without approval, with the channel on. linked
// connects their Telegram first.
func (e *env) accountUser(linked bool) (id string, csrf string) {
	e.t.Helper()
	e.signinChannel()
	id, secret := e.seedUser("acct@example.com", "viewer", true)
	if linked {
		e.linkTelegram(id)
	}
	e.login(e.c, "acct@example.com", secret)
	return id, e.authedCSRF(e.c)
}

func (e *env) userRow(id string) store.UserRow {
	e.t.Helper()
	u, err := e.db.UserByID(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) pinOf(id string) []byte { return e.userRow(id).SigninPINEnc }

func pinForm(csrf, pin, confirm string) url.Values {
	return url.Values{"csrf": {csrf}, "method": {"pin"}, "pin": {pin}, "confirm_pin": {confirm}}
}

func TestConnectingTelegramShowsAOneTimeLink(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, csrf := e.accountUser(false)
		_, page := e.get(e.c, "/dashboard/account")
		if !strings.Contains(page, "Connect Telegram") || strings.Contains(page, `name="method"`) {
			t.Fatalf("account page before linking:\n%s", page)
		}
		resp, page := e.post(e.c, linkRoute, url.Values{"csrf": {csrf}})
		if resp.StatusCode != 200 || !strings.Contains(page, `href="https://t.me/`+telegramtest.BotUsername+`?start=`) || !strings.Contains(page, "Open Telegram") {
			t.Fatalf("link: %d\n%s", resp.StatusCode, page)
		}
		if n := e.count(`SELECT COUNT(*) FROM telegram_links`); n != 1 {
			t.Fatalf("%d links kept", n)
		}
	})
}

func TestConnectingTelegramNeedsAWorkingBot(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, csrf := e.accountUser(false)
		e.tg.SetWebhook("https://example.com/hook")
		resp, page := e.post(e.c, linkRoute, url.Values{"csrf": {csrf}})
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "This bot has a webhook set.") {
			t.Fatalf("link: %d\n%s", resp.StatusCode, page)
		}
	})
}

func TestApprovalNeedsTelegramConnected(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(false)
		for _, form := range []url.Values{{"csrf": {csrf}, "method": {"tap"}}, pinForm(csrf, testPIN, testPIN)} {
			resp, body := e.post(e.c, approvalRoute, form)
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Connect your Telegram account first.") {
				t.Fatalf("%v: %d\n%s", form, resp.StatusCode, body)
			}
		}
		if u := e.userRow(id); u.SigninMethod != "" || u.SigninPINEnc != nil {
			t.Fatalf("stored anyway: %+v", u)
		}
	})
}

func TestTapIsTheDefaultMethod(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		_, page := e.get(e.c, "/dashboard/account")
		if !strings.Contains(page, `value="tap" checked`) || strings.Contains(page, `value="pin" checked`) || !strings.Contains(page, "Sign-in approval is off.") {
			t.Fatalf("account page:\n%s", page)
		}
		resp, body := e.post(e.c, approvalRoute, url.Values{"csrf": {csrf}})
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/account?n=approval" {
			t.Fatalf("set: %d\n%s", resp.StatusCode, body)
		}
		if u := e.userRow(id); u.SigninMethod != store.SigninMethodTap || !u.SigninApprovalOn() {
			t.Fatalf("user %+v", u)
		}
		if auditCount(t, db, "user.signin_approval_set") != 1 {
			t.Error("not audited")
		}
		_, page = e.get(e.c, "/dashboard/account?n=approval")
		if !strings.Contains(page, "Sign-in approval was saved.") || !strings.Contains(page, "tap <strong>Approve</strong>") || !strings.Contains(page, "Turn off sign-in approval") {
			t.Fatalf("account page:\n%s", page)
		}
	})
}

func TestSetPINStoresItEncryptedAndReplacesTap(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		if err := e.db.SetUserSigninTap(ctx, id, e.clk.Now()); err != nil {
			t.Fatal(err)
		}
		resp, body := e.post(e.c, approvalRoute, pinForm(csrf, testPIN, testPIN))
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/account?n=approval" {
			t.Fatalf("set: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		if u := e.userRow(id); u.SigninMethod != store.SigninMethodPIN {
			t.Fatalf("method %q", u.SigninMethod)
		}
		enc := e.pinOf(id)
		if len(enc) == 0 || bytes.Contains(enc, []byte(testPIN)) {
			t.Fatalf("stored %q", enc)
		}
		if plain, err := e.box.Open(enc, secrets.AADSigninPIN(id)); err != nil || string(plain) != testPIN {
			t.Fatalf("opened %q: %v", plain, err)
		}
		if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE "before" LIKE ? OR "after" LIKE ?`, "%"+testPIN+"%", "%"+testPIN+"%"); n != 0 {
			t.Error("the audit log holds the PIN")
		}
		_, page := e.get(e.c, "/dashboard/account")
		if !strings.Contains(page, `value="pin" checked`) || strings.Contains(page, testPIN) {
			t.Fatalf("account page:\n%s", page)
		}
		// Back to tap: the PIN is forgotten.
		if resp, _ := e.post(e.c, approvalRoute, url.Values{"csrf": {csrf}, "method": {"tap"}}); resp.StatusCode != 303 {
			t.Fatalf("tap: %d", resp.StatusCode)
		}
		if u := e.userRow(id); u.SigninMethod != store.SigninMethodTap || u.SigninPINEnc != nil {
			t.Fatalf("user %+v", u)
		}
	})
}

func TestPINFormatIsChecked(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		for _, tc := range [][2]string{{"12345", "12345"}, {"1234567890123", "1234567890123"}, {"12a456", "12a456"}, {"123456", "123457"}, {"", ""}} {
			resp, body := e.post(e.c, approvalRoute, pinForm(csrf, tc[0], tc[1]))
			if resp.StatusCode != http.StatusBadRequest || e.pinOf(id) != nil {
				t.Errorf("%v: %d, stored %q", tc, resp.StatusCode, e.pinOf(id))
			}
			if tc[0] != "" && strings.Contains(body, tc[0]) {
				t.Errorf("%v: the page shows the PIN", tc)
			}
		}
		if resp, _ := e.post(e.c, approvalRoute, url.Values{"csrf": {csrf}, "method": {"both"}}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unknown method: %d", resp.StatusCode)
		}
		if e.tg.Calls("getWebhookInfo") != 0 {
			t.Error("a bad form reached the bot check")
		}
		for _, good := range []string{"123456", "123456789012"} {
			if resp, _ := e.post(e.c, approvalRoute, pinForm(csrf, good, good)); resp.StatusCode != 303 {
				t.Errorf("%s refused: %d", good, resp.StatusCode)
			}
		}
	})
}

func TestApprovalRefusedWhenTheBotCannotDoIt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		e.tg.SetWebhook("https://example.com/hook")
		resp, body := e.post(e.c, approvalRoute, pinForm(csrf, testPIN, testPIN))
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "This bot has a webhook set.") || strings.Contains(body, testPIN) {
			t.Fatalf("refused: %d\n%s", resp.StatusCode, body)
		}
		if e.userRow(id).SigninMethod != "" {
			t.Fatal("stored anyway")
		}
	})
}

func TestTurningApprovalOffFromTheAccountPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		e.setPIN(id)
		resp, body := e.post(e.c, approvalRoute+"/off", url.Values{"csrf": {csrf}})
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/account?n=approval_off" {
			t.Fatalf("off: %d\n%s", resp.StatusCode, body)
		}
		if u := e.userRow(id); u.SigninMethod != "" || u.SigninPINEnc != nil || u.TelegramUserID == 0 {
			t.Fatalf("user %+v", u)
		}
		if auditCount(t, db, "user.signin_approval_removed") != 1 {
			t.Error("not audited")
		}
		_, page := e.get(e.c, "/dashboard/account?n=approval_off")
		if !strings.Contains(page, "Sign-in approval was turned off.") || strings.Contains(page, "Turn off sign-in approval") {
			t.Fatalf("account page:\n%s", page)
		}
	})
}

func TestDisconnectingTelegramTurnsApprovalOff(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		e.setPIN(id)
		resp, body := e.post(e.c, unlinkRoute, url.Values{"csrf": {csrf}})
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/account?n=unlinked" {
			t.Fatalf("unlink: %d\n%s", resp.StatusCode, body)
		}
		if u := e.userRow(id); u.TelegramUserID != 0 || u.SigninMethod != "" || u.SigninPINEnc != nil {
			t.Fatalf("user %+v", u)
		}
		if auditCount(t, db, "user.telegram_unlinked") != 1 {
			t.Error("not audited")
		}
		_, page := e.get(e.c, "/dashboard/account?n=unlinked")
		if !strings.Contains(page, "Telegram was disconnected") || !strings.Contains(page, "Connect Telegram") {
			t.Fatalf("account page:\n%s", page)
		}
	})
}

func TestTurningApprovalOffEndsPendingRequests(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		e.setPIN(id)
		other := e.client()
		u := e.userRow(id)
		plain, err := e.box.Open(u.TOTPSecretEnc, secrets.AADTOTP(id))
		if err != nil {
			t.Fatal(err)
		}
		e.nextCode()
		e.toWaiting(other, "acct@example.com", string(plain), false)
		if _, body := e.get(other, statusPath); body != "pending" {
			t.Fatalf("status %q", body)
		}
		e.post(e.c, approvalRoute+"/off", url.Values{"csrf": {csrf}})
		if _, body := e.get(other, statusPath); body != "ready" {
			t.Fatalf("after turning off: %q", body)
		}
		_, body := e.get(other, waitingPath)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("page:\n%s", body)
		}
	})
}

func TestApprovalChangesNeedARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser(true)
		e.clk.Add(11 * time.Hour)
		e.pageOK(e.c, "/dashboard/overview") // keep it alive (idle limit)
		e.clk.Add(2 * time.Hour)
		for _, route := range []string{approvalRoute, approvalRoute + "/off", linkRoute, unlinkRoute} {
			resp, _ := e.post(e.c, route, pinForm(csrf, testPIN, testPIN))
			if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/dashboard/reauth") {
				t.Fatalf("%s: %d %s", route, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		if u := e.userRow(id); u.SigninMethod != "" || u.TelegramUserID == 0 {
			t.Fatalf("changed without a recent sign-in: %+v", u)
		}
		if n := e.count(`SELECT COUNT(*) FROM telegram_links`); n != 0 {
			t.Fatalf("made a link without a recent sign-in: %d", n)
		}
	})
}
