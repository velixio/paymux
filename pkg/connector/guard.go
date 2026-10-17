package connector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrPrivateAddress means a request was refused because its target is, or
// resolves to, a loopback, private, link-local, multicast or unspecified
// address (see HTTPOptions.DenyPrivate).
var ErrPrivateAddress = errors.New("connector: target is a private or special-use address")

// IsPrivateIP reports whether ip is one a server-side request must not reach:
// loopback, RFC 1918 and ULA private ranges, carrier-grade NAT (100.64/10),
// link-local (169.254/16 including cloud metadata, fe80::/10), multicast,
// the unspecified address and 0.0.0.0/8.
func IsPrivateIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		if v4[0] == 0 || (v4[0] == 100 && v4[1]&0xc0 == 64) {
			return true
		}
	}
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

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
