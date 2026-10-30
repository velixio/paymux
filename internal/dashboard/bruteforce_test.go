package dashboard

import (
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// Sign-in guessing limits that hold even when the client address is chosen by
// the attacker (X-Forwarded-For through a trusted proxy): the counters that
// matter are per account and per ticket, not per address.

func bfEnv(t *testing.T, db *store.DB) *env {
	return newEnv(t, db, func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} })
}

func (e *env) postXFF(c *http.Client, path string, form url.Values, xff string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", xff)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) pwStep(c *http.Client, email, pw, xff string) (*http.Response, string) {
	e.t.Helper()
	_, page := e.get(c, "/dashboard/login")
	return e.postXFF(c, "/dashboard/login", url.Values{"email": {email}, "password": {pw}, "csrf": {csrfOf(e.t, page)}}, xff)
}

func (e *env) codeStep(c *http.Client, code, xff string) (*http.Response, string) {
	e.t.Helper()
	_, page := e.get(c, "/dashboard/login/totp")
	return e.postXFF(c, "/dashboard/login/totp", url.Values{"code": {code}, "csrf": {csrfOf(e.t, page)}}, xff)
}

func (e *env) goodCode(secret string) string { return totpFor(secret, e.clk.Now(), 0) }

func TestTicketDiesAfterFiveBadCodes(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		if r, _ := e.pwStep(c, "alice@example.com", goodPW, "198.51.100.1"); r.StatusCode != 303 {
			t.Fatalf("password step: %d", r.StatusCode)
		}
		for i := 0; i < 5; i++ {
			// Every guess comes from a new address: a per-address counter never trips.
			r, body := e.codeStep(c, fmt.Sprintf("00000%d", i), fmt.Sprintf("198.51.100.%d", 10+i))
			if i < 4 && (r.StatusCode != 200 || !strings.Contains(body, "Invalid code")) {
				t.Fatalf("bad code %d: %d", i, r.StatusCode)
			}
			if i == 4 && r.StatusCode != 303 {
				t.Fatalf("the fifth wrong code kills the ticket: %d", r.StatusCode)
			}
		}
		// The ticket is spent: even the right code does not complete the sign-in.
		_, loginPage := e.get(c, "/dashboard/login")
		r, _ := e.postXFF(c, "/dashboard/login/totp", url.Values{"code": {e.goodCode(secret)}, "csrf": {csrfOf(t, loginPage)}}, "198.51.100.99")
		if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/dashboard/login") {
			t.Errorf("a ticket with five failures still works: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		if sessionCookie(r) != nil {
			t.Error("a session was issued")
		}
	})
}

func TestTOTPFailuresAreCountedPerAccountWhateverTheAddress(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		_, secret := e.seedUser("alice@example.com", "admin", true)
		attempt := 0
		// Two tickets of five bad codes each, from ten different addresses.
		for ticket := 0; ticket < 2; ticket++ {
			c := e.client()
			if r, _ := e.pwStep(c, "alice@example.com", goodPW, fmt.Sprintf("203.0.113.%d", 100+ticket)); r.StatusCode != 303 {
				t.Fatalf("password step %d: %d", ticket, r.StatusCode)
			}
			for i := 0; i < 5; i++ {
				attempt++
				e.codeStep(c, fmt.Sprintf("11111%d", i), fmt.Sprintf("203.0.113.%d", attempt))
			}
		}
		// Ten failures: the account is locked for the window, for the password step too.
		c := e.client()
		if r, body := e.pwStep(c, "alice@example.com", goodPW, "192.0.2.77"); r.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many") {
			t.Fatalf("after ten failed codes the password step must refuse: %d", r.StatusCode)
		}
		// The window passes: a legitimate sign-in works.
		e.clk.Add(16 * time.Minute)
		if r, _ := e.pwStep(c, "alice@example.com", goodPW, "192.0.2.77"); r.StatusCode != 303 {
			t.Fatalf("after the window: %d", r.StatusCode)
		}
		if r, _ := e.codeStep(c, e.goodCode(secret), "192.0.2.77"); r.StatusCode != 303 || r.Header.Get("Location") != "/dashboard/" {
			t.Fatalf("legitimate sign-in after the window: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
	})
}

func TestPasswordFailuresAreCountedPerAccountWhateverTheAddress(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		for i := 0; i < 20; i++ {
			e.pwStep(c, "alice@example.com", "wrong password", fmt.Sprintf("198.51.%d.%d", i, i))
		}
		if r, _ := e.pwStep(c, "alice@example.com", goodPW, "192.0.2.200"); r.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("the correct password after 20 failures from 20 addresses: %d", r.StatusCode)
		}
		// An unknown account has a ceiling too (no oracle on which emails exist).
		for i := 0; i < 20; i++ {
			e.pwStep(c, "nobody@example.com", "x", fmt.Sprintf("198.51.%d.%d", i, i))
		}
		if r, _ := e.pwStep(c, "nobody@example.com", "x", "192.0.2.201"); r.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("unknown account: %d", r.StatusCode)
		}
		e.clk.Add(16 * time.Minute)
		if r, _ := e.pwStep(c, "alice@example.com", goodPW, "192.0.2.200"); r.StatusCode != 303 {
			t.Fatalf("after the window: %d", r.StatusCode)
		}
	})
}

func TestIPv6AddressesShareTheirSlash64(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := bfEnv(t, db)
		e.seedUser("alice@example.com", "admin", true)
		c := e.client()
		for i := 1; i <= 5; i++ {
			e.pwStep(c, "alice@example.com", "wrong", fmt.Sprintf("2001:db8:1:1::%x", i))
		}
		if r, _ := e.pwStep(c, "alice@example.com", goodPW, "2001:db8:1:1:aaaa:bbbb:cccc:dddd"); r.StatusCode != http.StatusTooManyRequests {
			t.Errorf("another address of the same /64 must share the budget: %d", r.StatusCode)
		}
		if r, _ := e.pwStep(c, "alice@example.com", goodPW, "2001:db8:1:2::1"); r.StatusCode != 303 {
			t.Errorf("a different /64 is a different client: %d", r.StatusCode)
		}
	})
}
