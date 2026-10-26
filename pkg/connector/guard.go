package connector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrPrivateAddress means a request was refused because its target is, or
// resolves to, a loopback, private, link-local, multicast or unspecified
// address (see HTTPOptions.DenyPrivate).
var ErrPrivateAddress = errors.New("connector: target is a private or special-use address")

// IsPrivateIP reports whether ip is one a server-side request must not reach.
// It is the one guard of the whole hub: this package's HTTP client, and the
// webhook and hook deliveries (internal/outbound), use it. It refuses every
// special-purpose range of IANA's registries (an IPv4-mapped IPv6 address is
// judged as the IPv4 address it carries; nil is refused):
//
//	IPv4  0.0.0.0/8, 10/8, 100.64/10 (carrier-grade NAT), 127/8, 169.254/16 (link
//	      local, cloud metadata), 172.16/12, 192.0.0.0/24, 192.0.2.0/24,
//	      192.88.99.0/24, 192.168/16, 198.18/15, 198.51.100.0/24, 203.0.113.0/24,
//	      224/4 (multicast), 240/4 (reserved, broadcast)
//	IPv6  ::/128, ::1, ::/96 (IPv4-compatible), 64:ff9b::/96 and 64:ff9b:1::/48
//	      (NAT64, which can reach private IPv4 behind a gateway), 100::/64,
//	      2001::/32 (Teredo), 2001:db8::/32, 2002::/16 (6to4), fc00::/7,
//	      fe80::/10, ff00::/8
func IsPrivateIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap().WithZone("")
	for _, p := range specialPurpose {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var specialPurpose = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
		"2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// guardedTransport dials only public addresses. The host is resolved here and
// every resulting address must be public (a name with one private address is
// refused); each connection is then checked again by the dialer's Control
// function against the address actually being connected to, so a resolver that
// answers differently the second time (DNS rebinding) cannot slip a private
// address in. Proxies from the environment are ignored: they would be the
// destination the guard never sees.
func guardedTransport(lookup func(ctx context.Context, host string) ([]net.IP, error)) *http.Transport {
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			out := make([]net.IP, len(addrs))
			for i, a := range addrs {
				out[i] = a.IP
			}
			return out, err
		}
	}
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil || IsPrivateIP(net.ParseIP(host)) {
				return fmt.Errorf("%w (%s)", ErrPrivateAddress, address)
			}
			return nil
		},
	}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips := []net.IP{net.ParseIP(host)}
			if ips[0] == nil {
				if ips, err = lookup(ctx, host); err != nil {
					return nil, err
				}
				if len(ips) == 0 {
					return nil, fmt.Errorf("connector: %s did not resolve", host)
				}
			}
			for _, ip := range ips {
				if IsPrivateIP(ip) {
					return nil, fmt.Errorf("%w (%s resolves to %s)", ErrPrivateAddress, host, ip)
				}
			}
			var last error
			for _, ip := range ips {
				c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			return nil, last
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
	}
}

// noRedirects makes the client return a 3xx response as it is instead of
// following it to a host the guard has not vetted.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
