package connector

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Addresses a server-side request must never reach.
func TestAudit2IsPrivateIPTable(t *testing.T) {
	block := []string{
		"127.0.0.1", "0.0.0.0", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "255.255.255.255",
		"::1", "::", "::ffff:127.0.0.1", "::ffff:7f00:1", "::ffff:169.254.169.254", "::127.0.0.1",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::7f00:1", "fd00::1", "fe80::1", "2002:7f00:1::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2",
	}
	for _, s := range block {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s is not refused", s)
		}
	}
	// Special-purpose space that is not in the list (informational: none is a
	// normal internal range, they matter only on unusual networks).
	gaps := []string{"fec0::1", "2001:1::1", "2001:3::1", "2001:20::1", "3fff::1", "5f00::1"}
	for _, s := range gaps {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s is not refused (IANA special-purpose range missing from the list)", s)
		}
	}
}

// DNS rebinding: the lookup answers a public address first and a private one
// for the dial. The guard resolves once and checks the same answer, then the
// Control hook checks the connected address.
func TestAudit2GuardRefusesMixedAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hit")) }))
	defer srv.Close()
	for name, ips := range map[string][]net.IP{
		"mapped":  {net.ParseIP("::ffff:127.0.0.1")},
		"mixed":   {net.ParseIP("93.184.216.34"), net.ParseIP("127.0.0.1")},
		"nat64":   {net.ParseIP("64:ff9b::7f00:1")},
		"decimal": {net.ParseIP("127.0.0.1")},
	} {
		c := NewHTTPClient(HTTPOptions{DenyPrivate: true, LookupIP: func(context.Context, string) ([]net.IP, error) { return ips, nil }})
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		resp, err := c.Get("http://rebind.example:" + port + "/")
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s: request went through", name)
			continue
		}
		if !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("%s: wrong error %v", name, err)
		}
	}
}

// Literal forms of loopback in the URL (decimal, octal, hex, short) must not get through.
func TestAudit2GuardRefusesAlternateLiterals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hit")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := NewHTTPClient(HTTPOptions{DenyPrivate: true})
	for _, h := range []string{"2130706433", "0x7f000001", "017700000001", "127.1", "0", "[::ffff:127.0.0.1]", "[::ffff:7f00:1]", "localhost", "127.0.0.1.", "0177.0.0.1"} {
		resp, err := c.Get("http://" + h + ":" + port + "/")
		if err == nil {
			resp.Body.Close()
			t.Errorf("%s: request went through", h)
		}
	}
}

// A redirect must not be followed by a DenyPrivate client.
func TestAudit2GuardDoesNotFollowRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("internal")) }))
	defer target.Close()
	c := NewHTTPClient(HTTPOptions{DenyPrivate: true, LookupIP: func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}})
	_ = c
}

// The default (non-guarded) client used by every provider connector follows
// redirects, and Go forwards custom headers (Cashfree's x-client-secret) to the
// new host. Only Authorization/Cookie are stripped.
func TestAudit2DefaultClientForwardsCustomHeaderOnRedirect(t *testing.T) {
	var got string
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("X-Client-Secret") }))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+b.Listener.Addr().(*net.TCPAddr).AddrPort().String()[len("127.0.0.1:"):]+"/", http.StatusFound)
	}))
	defer a.Close()
	c := NewHTTPClient(HTTPOptions{})
	req, _ := http.NewRequest("GET", a.URL, nil)
	req.Header.Set("X-Client-Secret", "s3cret")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "" {
		t.Errorf("custom secret header was forwarded to the redirect target: %q", got)
	}
}
