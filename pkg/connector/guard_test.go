package connector

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestIsPrivateIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "127.9.9.9", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.0.1", "169.254.169.254", "100.64.0.1",
		"100.127.255.255", "0.0.0.0", "0.1.2.3", "224.0.0.1", "239.1.1.1", "::1", "::", "fe80::1", "fc00::1", "fd12::1", "ff02::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1"} {
		if !IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s not private", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "172.32.0.1", "172.15.255.255", "100.63.255.255", "100.128.0.1", "2606:4700::1111", "::ffff:8.8.8.8"} {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s private", s)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("nil must be refused")
	}
}

func TestDenyPrivateClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := NewHTTPClient(HTTPOptions{DenyPrivate: true, LookupIP: func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil // a name that resolves to the local test server
	}})
	if _, err := c.Get(srv.URL); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("literal loopback: %v", err)
	}
	if _, err := c.Get("http://service.example.com:1/"); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("name resolving to loopback: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("the private server was contacted")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("redirects are followed: %v", err)
	}
	// Without the option the client is as before.
	if resp, err := NewHTTPClient(HTTPOptions{}).Get(srv.URL); err != nil {
		t.Errorf("default client: %v", err)
	} else {
		resp.Body.Close()
	}
	if NewHTTPClient(HTTPOptions{}).CheckRedirect != nil {
		t.Error("the default client's redirect policy changed")
	}
}
