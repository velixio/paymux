package connector

import (
	"net"
	"net/netip"
	"testing"
)

// Every special-purpose range a server-side request must not reach (IANA's
// special-purpose registries), each with its first, a middle and its last address,
// and the IPv4-mapped IPv6 form of every IPv4 one. One shared function guards the
// connectors' HTTP client and the webhook and hook deliveries.
var specialRanges = []string{
	// IPv4
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	// IPv6
	"::/128", "::1/128", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32",
	"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
}

func edges(p netip.Prefix) []netip.Addr {
	first := p.Masked().Addr()
	last := first.AsSlice()
	bits := p.Bits()
	for i := bits; i < len(last)*8; i++ {
		last[i/8] |= 1 << (7 - uint(i%8))
	}
	l, _ := netip.AddrFromSlice(last)
	mid := first.AsSlice()
	if bits < len(mid)*8-1 {
		i := bits
		mid[i/8] |= 1 << (7 - uint(i%8)) // the next bit up, still inside the prefix
		mid[len(mid)-1] ^= 0x01
	}
	m, _ := netip.AddrFromSlice(mid)
	out := []netip.Addr{first, l}
	if p.Contains(m) {
		out = append(out, m)
	}
	return out
}

func TestEverySpecialPurposeRangeIsPrivate(t *testing.T) {
	for _, cidr := range specialRanges {
		p := netip.MustParsePrefix(cidr)
		for _, a := range edges(p) {
			if !IsPrivateIP(net.IP(a.AsSlice())) {
				t.Errorf("%s (in %s) is not refused", a, cidr)
			}
			if a.Is4() {
				// The same address as an IPv4-mapped IPv6 one (::ffff:a.b.c.d).
				mapped := netip.AddrFrom16(a.As16())
				if !IsPrivateIP(net.IP(mapped.AsSlice())) {
					t.Errorf("%s (the mapped form of %s, in %s) is not refused", mapped, a, cidr)
				}
			}
		}
	}
}

func TestPublicAddressesAreAllowed(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "100.63.255.255", "100.128.0.0", "172.15.255.255", "172.32.0.0", "192.0.1.1",
		"198.17.255.255", "198.20.0.0", "223.255.255.255", "2606:4700:4700::1111", "2a00:1450:4001:81b::200e", "2001:4860:4860::8888",
		"2003::1", "::ffff:8.8.8.8", "64:ff9a::1"} {
		if IsPrivateIP(net.ParseIP(s)) {
			t.Errorf("%s is refused but is public", s)
		}
	}
	if !IsPrivateIP(nil) {
		t.Error("a nil address must be refused")
	}
}
