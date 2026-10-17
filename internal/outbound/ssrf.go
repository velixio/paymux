package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// DeliveryTimeout bounds one delivery attempt.
const DeliveryTimeout = 10 * time.Second

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isBlockedIP reports whether ip is loopback, private, link-local, multicast,
// unspecified or carrier-grade NAT (IPv4-mapped IPv6 is unmapped first).
func isBlockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || cgnat.Contains(a)
}

// ErrPrivateAddress means the target resolved to a forbidden address.
var ErrPrivateAddress = errors.New("webhook target resolves to a private address")

type lookupFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

// newHTTPClient builds the delivery client. Every resolved IP is checked at
// dial time (not before), and the connection goes to the vetted IP itself, so
// DNS rebinding cannot swap in a private address. Redirects are not followed.
// A nil lookup uses the system resolver.
func newHTTPClient(allowPrivate bool, lookup lookupFunc) *http.Client {
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	d := &net.Dialer{Timeout: DeliveryTimeout}
	tr := &http.Transport{
		Proxy: nil, // a proxy would dial on our behalf and bypass the guard
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := lookup(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no addresses for %s", host)
			}
			if !allowPrivate {
				for _, ip := range ips {
					if isBlockedIP(ip.IP) {
						return nil, fmt.Errorf("%w (%s)", ErrPrivateAddress, host)
					}
				}
			}
			var lastErr error
			for _, ip := range ips {
				c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return c, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   DeliveryTimeout,
		ResponseHeaderTimeout: DeliveryTimeout,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       DeliveryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// GuardedClient is the SSRF-guarded HTTP client (private, loopback and
// link-local targets refused at dial time, redirects not followed) with the
// given overall request timeout. JavaScript hooks use it for fetch.
func GuardedClient(allowPrivate bool, timeout time.Duration) *http.Client {
	c := newHTTPClient(allowPrivate, nil)
	c.Timeout = timeout
	return c
}
