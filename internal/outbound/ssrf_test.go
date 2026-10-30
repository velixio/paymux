package outbound

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.9.9.9", "::1", "10.0.0.1", "172.16.5.5", "192.168.1.1",
		"169.254.169.254", "fe80::1", "fc00::1", "0.0.0.0", "::", "100.64.0.1",
		"::ffff:127.0.0.1", "::ffff:10.1.2.3", "224.0.0.1", "ff02::1",
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

// The delivery guard and the connectors' HTTP guard are one function
// (connector.IsPrivateIP); the ranges the old copy missed are refused here too:
// the rest of 0/8, TEST-NETs, 198.18/15, reserved 240/4, NAT64, 6to4, and the
// IPv4-mapped form of each.
func TestIsBlockedIPIsTheSharedGuard(t *testing.T) {
	for _, s := range []string{"0.1.2.3", "192.0.0.9", "192.0.2.1", "192.88.99.1", "198.18.0.1", "198.19.255.254", "198.51.100.7", "203.0.113.200",
		"240.0.0.1", "255.255.255.255", "::ffff:198.18.0.1", "::ffff:203.0.113.5", "::ffff:240.0.0.1", "::ffff:0.1.2.3",
		"64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b:1::1", "2002:7f00:1::1", "2002:c0a8:101::1", "2001:db8::1", "2001:0:4136:e378::1", "100::1", "::7f00:1"} {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "::ffff:8.8.8.8", "2606:4700:4700::1111", "198.20.0.1", "100.128.0.1"} {
		if isBlockedIP(net.ParseIP(s)) != connector.IsPrivateIP(net.ParseIP(s)) || isBlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed by both guards", s)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("a nil address must be blocked")
	}
}

func fakeLookup(m map[string][]string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IPAddr{{IP: ip}}, nil
		}
		ips, ok := m[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []net.IPAddr
		for _, s := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(s)})
		}
		return out, nil
	}
}

func TestClientBlocksPrivateTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	lookup := fakeLookup(map[string][]string{
		"rebind.test": {"127.0.0.1"},
		"mixed.test":  {"93.184.216.34", "127.0.0.1"},
		"localhost":   {"127.0.0.1", "::1"},
	})
	strict := newHTTPClient(false, lookup)
	for _, host := range []string{"127.0.0.1", "rebind.test", "mixed.test", "localhost"} {
		_, err := strict.Get("http://" + net.JoinHostPort(host, port) + "/")
		if err == nil || !strings.Contains(err.Error(), "private") {
			t.Errorf("%s: want private-address error, got %v", host, err)
		}
	}

	open := newHTTPClient(true, lookup)
	for _, host := range []string{"127.0.0.1", "rebind.test"} {
		resp, err := open.Get("http://" + net.JoinHostPort(host, port) + "/")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()
	resp, err := newHTTPClient(true, nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d, redirect was followed", resp.StatusCode)
	}
}

func TestGuardedClientIsTheSameGuardWithYourTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	c := GuardedClient(false, 3*time.Second)
	if c.Timeout != 3*time.Second {
		t.Errorf("timeout %v", c.Timeout)
	}
	if _, err := c.Get(srv.URL); err == nil || !strings.Contains(err.Error(), ErrPrivateAddress.Error()) {
		t.Errorf("loopback must be refused: %v", err)
	}
	if res, err := GuardedClient(true, time.Second).Get(srv.URL); err != nil || res.StatusCode != 200 {
		t.Errorf("allow-private client: %v", err)
	}
}
