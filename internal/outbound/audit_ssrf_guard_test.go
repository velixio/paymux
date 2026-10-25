package outbound

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Audit 2026-10 (expected to pass): every resolved address is judged, mapped
// and NAT64 forms included, a redirect is not followed, and HTTP_PROXY is not
// used.
func TestAuditOutboundGuard(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, ip := range []string{"127.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "100.64.0.1",
		"0.0.0.0", "::", "fd00::1", "fe80::1", "::1", "2002:7f00:1::1", "::127.0.0.1"} {
		c := newHTTPClient(false, func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP(ip)}}, nil
		})
		_, err := c.Get("http://victim.example/")
		if !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("%s: not refused: %v", ip, err)
		}
	}
	// redirect to an internal address is returned, not followed
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL, http.StatusFound)
	}))
	defer outer.Close()
	c := newHTTPClient(true, nil)
	resp, err := c.Get(outer.URL)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("got %v %v", resp, err)
	}
	if !strings.HasPrefix(resp.Header.Get("Location"), inner.URL) {
		t.Fatal("unexpected location")
	}
}
