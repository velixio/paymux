// Package clientip names the client of an HTTP request behind reverse proxies.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// From is the address of the client. X-Forwarded-For counts only when the
// connection comes from one of the trusted proxies; the list is then read from
// the right, skipping trusted proxies, so entries the client made up on the
// left are never believed.
func From(r *http.Request, trusted []netip.Prefix) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	peer, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	if !isTrusted(peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" && len(hops) == 1 {
			break
		}
		a, err := netip.ParseAddr(hop)
		if err != nil {
			break // not an address: do not guess
		}
		if a = a.Unmap(); !isTrusted(a) {
			return a.String()
		}
	}
	return peer.String()
}

// Group is the address, or for IPv6 its /64 prefix: one subscriber usually
// holds a whole /64, so limits keyed by address would be dodged by rotating
// through it. IPv4, IPv4-in-IPv6 and anything unparsable are returned as is.
func Group(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || !a.Is6() || a.Is4In6() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}
