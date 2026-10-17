package outbound

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func fakeLookup(m map[string][]string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
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
