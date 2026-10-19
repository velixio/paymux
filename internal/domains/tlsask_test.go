package domains

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
)

type fakeResolver struct {
	mu    sync.Mutex
	addrs map[string][]netip.Addr
	asked []string
}

func (f *fakeResolver) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, host)
	a, ok := f.addrs[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return a, nil
}

func ips(s ...string) []netip.Addr {
	var out []netip.Addr
	for _, x := range s {
		out = append(out, netip.MustParseAddr(x))
	}
	return out
}

func ask(h *TLSAllow, domain string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/internal/tls-allow?domain="+domain, nil))
	return w.Code
}

// Caddy gets a certificate for whatever this handler approves, and Let's
// Encrypt limits how many it hands out. Anyone who can register a domain in
// the dashboard must not be able to spend them on names that are not pointed at
// the hub: a verified domain is approved, a pending one only when its DNS
// already leads here.
func TestTLSAskApprovesVerifiedDomainsAndPendingOnesThatPointHere(t *testing.T) {
	res := &fakeResolver{addrs: map[string][]netip.Addr{
		"pay.example.com":         ips("203.0.113.10", "2001:db8::10"),
		"pay.pending.example":     ips("203.0.113.10"),
		"pay.pendingv6.example":   ips("2001:db8::10"),
		"pay.elsewhere.example":   ips("198.51.100.7"),
		"pay.mixed.example":       ips("198.51.100.7", "203.0.113.10"),
		"pay.voicestudio.example": ips("198.51.100.99"), // verified: where it points is its owner's business
	}}
	lookup := fakeLookup{
		"pay.voicestudio.example": {AppID: "app_vs", Status: StatusActive},
		"pay.pending.example":     {AppID: "a", Status: StatusPending},
		"pay.pendingv6.example":   {AppID: "a", Status: StatusPending},
		"pay.elsewhere.example":   {AppID: "a", Status: StatusPending},
		"pay.mixed.example":       {AppID: "a", Status: StatusPending},
		"pay.nodns.example":       {AppID: "a", Status: StatusPending},
		"pay.bad.example":         {AppID: "a", Status: StatusFailed},
	}
	h := NewTLSAllow(TLSAllowOptions{BaseHost: "pay.example.com", Lookup: lookup, Resolve: res.resolve})
	for domain, want := range map[string]int{
		"pay.example.com":         200, // the hub itself
		"pay.voicestudio.example": 200, // verified
		"pay.pending.example":     200, // pending, A record is the hub's
		"pay.pendingv6.example":   200, // pending, AAAA record is the hub's
		"pay.mixed.example":       200, // one of its addresses is the hub's
		"pay.elsewhere.example":   403, // pending, points somewhere else
		"pay.nodns.example":       403, // pending, does not resolve
		"pay.bad.example":         403, // failed verification
		"evil.example":            403, // unknown
		"":                        400,
	} {
		if got := ask(h, domain); got != want {
			t.Errorf("%q: %d, want %d", domain, got, want)
		}
	}
	// A verified domain costs no DNS lookup.
	res.mu.Lock()
	for _, host := range res.asked {
		if host == "pay.voicestudio.example" || host == "evil.example" {
			t.Errorf("resolved %s, which needs no lookup", host)
		}
	}
	res.mu.Unlock()
}

// If the hub's own address cannot be found out, no pending domain is approved.
func TestTLSAskFailsClosedWhenTheHubsAddressIsUnknown(t *testing.T) {
	res := &fakeResolver{addrs: map[string][]netip.Addr{"pay.pending.example": ips("203.0.113.10")}}
	h := NewTLSAllow(TLSAllowOptions{BaseHost: "pay.example.com", Lookup: fakeLookup{"pay.pending.example": {AppID: "a", Status: StatusPending}}, Resolve: res.resolve})
	if got := ask(h, "pay.pending.example"); got != 403 {
		t.Errorf("pending with an unresolvable hub: %d", got)
	}
	if got := ask(h, "pay.example.com"); got != 200 {
		t.Errorf("the base host itself: %d", got)
	}
}
