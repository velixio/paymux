package clientip

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestFrom(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.5:1234", "198.51.100.1", "203.0.113.5"},          // not a proxy: the header is not believed
		{"10.0.0.2:1234", "198.51.100.1", "198.51.100.1"},            // a proxy: the client it names
		{"10.0.0.2:1234", "6.6.6.6, 198.51.100.1", "198.51.100.1"},   // what the client prepended is not believed
		{"10.0.0.2:1234", "198.51.100.1, 10.0.0.9", "198.51.100.1"},  // proxies on the right are skipped
		{"10.0.0.2:1234", "not-an-address", "10.0.0.2"},              // junk: no guess
		{"10.0.0.2:1234", "", "10.0.0.2"},                            // no header
		{"[::1]:1234", "2001:db8::7", "2001:db8::7"},                 // IPv6
		{"[::ffff:203.0.113.5]:1234", "198.51.100.1", "203.0.113.5"}, // v4-mapped peer
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := From(r, trusted); got != c.want {
			t.Errorf("%s via %q: got %s want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestGroup(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":          "203.0.113.5",
		"::ffff:203.0.113.5":   "::ffff:203.0.113.5",
		"2001:db8:1:2:aaaa::1": "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"junk":                 "junk",
	} {
		if got := Group(in); got != want {
			t.Errorf("Group(%q) = %q, want %q", in, got, want)
		}
	}
}
