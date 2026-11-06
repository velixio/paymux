package dashboard

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const signinPINRoute = "/dashboard/account/signin-pin"

// accountUser signs in a user without a PIN, with the channel on.
func (e *env) accountUser() (id string, csrf string) {
	e.t.Helper()
	e.signinChannel()
	id, secret := e.seedUser("acct@example.com", "viewer", true)
	e.login(e.c, "acct@example.com", secret)
	return id, e.authedCSRF(e.c)
}

func (e *env) pinOf(id string) []byte {
	e.t.Helper()
	u, err := e.db.UserByID(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.SigninPINEnc
}

func pinForm(csrf, pin, confirm string) url.Values {
	return url.Values{"csrf": {csrf}, "pin": {pin}, "confirm_pin": {confirm}}
}

func TestSetPINStoresItEncrypted(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser()
		resp, body := e.post(e.c, signinPINRoute, pinForm(csrf, testPIN, testPIN))
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/account?n=pin" {
			t.Fatalf("set: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
		}
		enc := e.pinOf(id)
		if len(enc) == 0 || bytes.Contains(enc, []byte(testPIN)) {
			t.Fatalf("stored %q", enc)
		}
		if plain, err := e.box.Open(enc, secrets.AADSigninPIN(id)); err != nil || string(plain) != testPIN {
			t.Fatalf("opened %q: %v", plain, err)
		}
		if auditCount(t, db, "user.signin_pin_set") != 1 {
			t.Error("not audited")
		}
		if n := e.count(`SELECT COUNT(*) FROM audit_log WHERE "before" LIKE ? OR "after" LIKE ?`, "%"+testPIN+"%", "%"+testPIN+"%"); n != 0 {
			t.Error("the audit log holds the PIN")
		}
		_, page := e.get(e.c, "/dashboard/account?n=pin")
		if !strings.Contains(page, "Your sign-in PIN was saved.") || !strings.Contains(page, "Remove PIN") || strings.Contains(page, testPIN) {
			t.Fatalf("account page:\n%s", page)
		}
	})
}

func TestPINFormatIsChecked(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser()
		for _, tc := range [][2]string{{"12345", "12345"}, {"1234567890123", "1234567890123"}, {"12a456", "12a456"}, {"123456", "123457"}, {"", ""}} {
			resp, body := e.post(e.c, signinPINRoute, pinForm(csrf, tc[0], tc[1]))
			if resp.StatusCode != http.StatusBadRequest || e.pinOf(id) != nil {
				t.Errorf("%v: %d, stored %q", tc, resp.StatusCode, e.pinOf(id))
			}
			if tc[0] != "" && strings.Contains(body, tc[0]) {
				t.Errorf("%v: the page shows the PIN", tc)
			}
		}
		if e.tg.Calls("getWebhookInfo") != 0 {
			t.Error("a bad PIN reached the channel check")
		}
		for _, good := range []string{"123456", "123456789012"} {
			if resp, _ := e.post(e.c, signinPINRoute, pinForm(csrf, good, good)); resp.StatusCode != 303 {
				t.Errorf("%s refused: %d", good, resp.StatusCode)
			}
		}
	})
}

func TestSetPINRefusedWhenTheChatCannotDoIt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser()
		e.tg.SetChat(-100123, "channel")
		resp, body := e.post(e.c, signinPINRoute, pinForm(csrf, testPIN, testPIN))
		// The page escapes the apostrophe.
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "because they have no normal replies") || strings.Contains(body, testPIN) {
			t.Fatalf("refused: %d\n%s", resp.StatusCode, body)
		}
		if e.pinOf(id) != nil {
			t.Fatal("stored anyway")
		}
	})
}

func TestRemovePINFromTheAccountPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser()
		e.setPIN(id)
		resp, body := e.post(e.c, signinPINRoute+"/remove", url.Values{"csrf": {csrf}})
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/dashboard/account?n=pin_removed" {
			t.Fatalf("remove: %d\n%s", resp.StatusCode, body)
		}
		if e.pinOf(id) != nil {
			t.Fatal("still set")
		}
		if auditCount(t, db, "user.signin_pin_removed") != 1 {
			t.Error("not audited")
		}
		_, page := e.get(e.c, "/dashboard/account?n=pin_removed")
		if !strings.Contains(page, "Your sign-in PIN was removed.") || strings.Contains(page, "Remove PIN") {
			t.Fatalf("account page:\n%s", page)
		}
	})
}

func TestRemovingThePINEndsPendingRequests(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser()
		e.setPIN(id)
		other := e.client()
		u, _ := e.db.UserByID(ctx, id)
		if u.TOTPSecretEnc == nil {
			t.Fatal("no secret")
		}
		plain, err := e.box.Open(u.TOTPSecretEnc, secrets.AADTOTP(id))
		if err != nil {
			t.Fatal(err)
		}
		e.nextCode()
		e.toWaiting(other, "acct@example.com", string(plain), false)
		if _, body := e.get(other, statusPath); body != "pending" {
			t.Fatalf("status %q", body)
		}
		e.post(e.c, signinPINRoute+"/remove", url.Values{"csrf": {csrf}})
		if _, body := e.get(other, statusPath); body != "ready" {
			t.Fatalf("after removal: %q", body)
		}
		_, body := e.get(other, waitingPath)
		if !strings.Contains(body, endedWords) {
			t.Fatalf("page:\n%s", body)
		}
	})
}

func TestPINChangesNeedARecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		id, csrf := e.accountUser()
		e.clk.Add(11 * time.Hour)
		e.pageOK(e.c, "/dashboard/overview") // keep it alive (idle limit)
		e.clk.Add(2 * time.Hour)
		resp, _ := e.post(e.c, signinPINRoute, pinForm(csrf, testPIN, testPIN))
		if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/dashboard/reauth") {
			t.Fatalf("set: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		if e.pinOf(id) != nil {
			t.Fatal("stored without a recent sign-in")
		}
		e.setPIN(id)
		resp, _ = e.post(e.c, signinPINRoute+"/remove", url.Values{"csrf": {csrf}})
		if resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/dashboard/reauth") {
			t.Fatalf("remove: %d %s", resp.StatusCode, resp.Header.Get("Location"))
		}
		if e.pinOf(id) == nil {
			t.Fatal("removed without a recent sign-in")
		}
	})
}
