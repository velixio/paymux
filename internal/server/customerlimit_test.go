package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func limited(t *testing.T, perMinute int, now *time.Time) http.Handler {
	t.Helper()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}
	l := NewCustomerLimit(perMinute, trusted)
	l.now = func() time.Time { return *now }
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	return l.Wrap(ok)
}

func hit(h http.Handler, path, remote, xff string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCustomerLimitPerAddressAndFamily(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := limited(t, 60, &now)
	for i := 0; i < 20; i++ {
		if w := hit(h, "/pay/pay_"+strconv.Itoa(i), "203.0.113.5:1000", ""); w.Code != 204 {
			t.Fatalf("burst request %d = %d", i, w.Code)
		}
	}
	w := hit(h, "/pay/pay_x", "203.0.113.5:1000", "")
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over burst = %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if hit(h, "/l/slug", "203.0.113.5:1000", "").Code != 204 {
		t.Fatal("another family shares the pay bucket")
	}
	if hit(h, "/pay/pay_x", "203.0.113.6:1000", "").Code != 204 {
		t.Fatal("another address shares the bucket")
	}
	if hit(h, "/dashboard/", "203.0.113.5:1000", "").Code != 204 || hit(h, "/v1/payments", "203.0.113.5:1000", "").Code != 204 {
		t.Fatal("a route outside the customer families was limited")
	}
	now = now.Add(2 * time.Second) // 60/min refills 2 tokens
	if hit(h, "/pay/pay_x", "203.0.113.5:1000", "").Code != 204 {
		t.Fatal("the bucket did not refill")
	}
}

func TestCustomerLimitFamilies(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := limited(t, 60, &now)
	for _, p := range []string{"/pay/a", "/return/a/b", "/l/a", "/s/a", "/i/a"} {
		for i := 0; i < 20; i++ {
			if hit(h, p, "203.0.113.5:1", "").Code != 204 {
				t.Fatalf("%s request %d limited early", p, i)
			}
		}
		if hit(h, p, "203.0.113.5:1", "").Code != 429 {
			t.Fatalf("%s not limited", p)
		}
	}
}

func TestCustomerLimitForwardedFor(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := limited(t, 60, &now)
	// A trusted proxy: the forwarded address is the key.
	for i := 0; i < 20; i++ {
		hit(h, "/pay/a", "10.0.0.1:1", "198.51.100.7")
	}
	if hit(h, "/pay/a", "10.0.0.1:1", "198.51.100.7").Code != 429 {
		t.Fatal("forwarded client not limited")
	}
	if hit(h, "/pay/a", "10.0.0.1:1", "198.51.100.8").Code != 204 {
		t.Fatal("trusted proxy's other client shares the bucket")
	}
	// An untrusted peer cannot pick its key: rotating the header does not help.
	for i := 0; i < 20; i++ {
		hit(h, "/pay/a", "203.0.113.9:1", "192.0.2."+strconv.Itoa(i+1))
	}
	if hit(h, "/pay/a", "203.0.113.9:1", "192.0.2.200").Code != 429 {
		t.Fatal("an untrusted X-Forwarded-For was honoured")
	}
}

func TestCustomerLimitDisabled(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := limited(t, 0, &now)
	for i := 0; i < 500; i++ {
		if hit(h, "/pay/a", "203.0.113.5:1", "").Code != 204 {
			t.Fatal("limited although disabled")
		}
	}
}

func TestCustomerLimitPrunesIdleKeys(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewCustomerLimit(60, nil)
	l.now = func() time.Time { return now }
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for i := 0; i < 50; i++ {
		hit(h, "/pay/a", "198.51.100."+strconv.Itoa(i)+":1", "")
	}
	now = now.Add(10 * time.Minute)
	hit(h, "/pay/a", "203.0.113.1:1", "")
	if n := l.size(); n != 1 {
		t.Fatalf("%d keys kept after idle, want 1", n)
	}
}

func TestCustomerLimitIPv6SharesItsPrefix(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	h := limited(t, 60, &now)
	for i := 0; i < 20; i++ {
		hit(h, "/pay/a", "[2001:db8:1:2::"+strconv.Itoa(i+1)+"]:1", "")
	}
	if hit(h, "/pay/a", "[2001:db8:1:2:ffff::9]:1", "").Code != 429 {
		t.Fatal("addresses in one /64 got separate buckets")
	}
	if hit(h, "/pay/a", "[2001:db8:1:3::1]:1", "").Code != 204 {
		t.Fatal("another /64 shares the bucket")
	}
}
