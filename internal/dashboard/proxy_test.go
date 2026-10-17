package dashboard

import (
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func prefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

func TestClientIPHonoursForwardedForOnlyFromTrustedProxies(t *testing.T) {
	trusted := prefixes(t, "10.0.0.0/8", "192.168.1.5/32")
	cases := []struct {
		name, remote, xff, want string
		trusted                 []netip.Prefix
	}{
		{"no proxies configured: header ignored", "203.0.113.9:5000", "198.51.100.7", "203.0.113.9", nil},
		{"untrusted peer: header ignored", "203.0.113.9:5000", "198.51.100.7", "203.0.113.9", trusted},
		{"trusted peer, one hop", "10.1.2.3:5000", "198.51.100.7", "198.51.100.7", trusted},
		{"trusted peer, no header", "10.1.2.3:5000", "", "10.1.2.3", trusted},
		{"client-supplied prefix is not believed", "10.1.2.3:5000", "6.6.6.6, 198.51.100.7", "198.51.100.7", trusted},
		{"chain of trusted proxies", "10.1.2.3:5000", "198.51.100.7, 10.9.9.9, 192.168.1.5", "198.51.100.7", trusted},
		{"all hops trusted: fall back to the peer", "10.1.2.3:5000", "10.9.9.9", "10.1.2.3", trusted},
		{"garbage entry stops the walk", "10.1.2.3:5000", "198.51.100.7, not-an-ip", "10.1.2.3", trusted},
		{"ipv6 client", "10.1.2.3:5000", "2001:db8::1", "2001:db8::1", trusted},
		{"ipv4-mapped peer", "[::ffff:10.1.2.3]:5000", "198.51.100.7", "198.51.100.7", trusted},
		{"peer without a port", "10.1.2.3", "198.51.100.7", "198.51.100.7", trusted},
		{"several header lines", "10.1.2.3:5000", "", "198.51.100.7", trusted},
	}
	for _, c := range cases {
		d := &Dashboard{o: Options{TrustedProxies: c.trusted}}
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.name == "several header lines" {
			r.Header.Add("X-Forwarded-For", "6.6.6.6")
			r.Header.Add("X-Forwarded-For", "198.51.100.7")
		}
		if got := d.clientIP(r); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func (e *env) loginAttempt(c *http.Client, email, xff string) int {
	e.t.Helper()
	_, page := e.get(c, "/dashboard/login")
	req, _ := http.NewRequest("POST", e.srv.URL+"/dashboard/login", strings.NewReader(url.Values{"email": {email}, "password": {"wrong password here"}, "csrf": {csrfOf(e.t, page)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestLoginLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db) // no trusted proxies
		e.seedUser("alice@example.com", "admin", true)
		for i := 0; i < 5; i++ {
			e.loginAttempt(e.c, "alice@example.com", "198.51.100."+string(rune('1'+i))) // a new "client" each time
		}
		if code := e.loginAttempt(e.c, "alice@example.com", "198.51.100.99"); code != 429 {
			t.Fatalf("a spoofed X-Forwarded-For evaded the lockout: %d", code)
		}
	})
}

func TestLoginLimitAndAuditUseTheForwardedClientBehindATrustedProxy(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(o *Options) { o.TrustedProxies = prefixes(t, "127.0.0.1/32") }) // the test client connects from loopback
		e.seedUser("alice@example.com", "admin", true)
		for i := 0; i < 5; i++ {
			e.loginAttempt(e.c, "alice@example.com", "198.51.100.7")
		}
		if code := e.loginAttempt(e.c, "alice@example.com", "198.51.100.7"); code != 429 {
			t.Fatalf("the same forwarded client should be locked out: %d", code)
		}
		// another client behind the same proxy is unaffected
		if code := e.loginAttempt(e.client(), "alice@example.com", "198.51.100.8"); code != 200 {
			t.Fatalf("a different forwarded client was locked out too: %d", code)
		}
	})
}

func TestSessionAndAuditRowsRecordTheForwardedClient(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(o *Options) { o.TrustedProxies = prefixes(t, "127.0.0.1/32") })
		_, secret := e.seedUser("bob@example.com", "admin", true)
		c := e.client()
		withXFF := func(path string, form url.Values) *http.Response {
			req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Forwarded-For", "203.0.113.50")
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp
		}
		_, page := e.get(c, "/dashboard/login")
		withXFF("/dashboard/login", url.Values{"email": {"bob@example.com"}, "password": {goodPW}, "csrf": {csrfOf(t, page)}})
		_, page = e.get(c, "/dashboard/login/totp")
		resp := withXFF("/dashboard/login/totp", url.Values{"code": {totpFor(secret, e.clk.Now(), 0)}, "csrf": {csrfOf(t, page)}})
		if sessionCookie(resp) == nil {
			t.Fatalf("login failed: %d", resp.StatusCode)
		}
		var ip string
		_ = db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, `SELECT ip FROM sessions`).Scan(&ip)
		})
		if ip != "203.0.113.50" {
			t.Fatalf("session ip %q", ip)
		}
		tok := e.authedCSRF(c)
		withXFF("/dashboard/users", url.Values{"email": {"new@example.com"}, "role": {"viewer"}, "password": {goodPW}, "csrf": {tok}})
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "user.create", 5)
			return
		})
		if len(aud) != 1 || aud[0].IP != "203.0.113.50" {
			t.Fatalf("audit %+v", aud)
		}
	})
}
