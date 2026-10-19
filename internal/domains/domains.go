// Package domains routes requests by Host: the base host serves everything,
// app custom domains serve only customer-facing routes (§6.2), unknown hosts
// get 421. It also answers Caddy's on-demand TLS "ask" requests.
package domains

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// Domain statuses.
const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusFailed  = "failed"
)

// Domain is an app custom domain.
type Domain struct {
	AppID  string
	Status string
}

// Lookup finds a custom domain by normalised host.
type Lookup interface {
	LookupHost(ctx context.Context, host string) (Domain, bool, error)
}

type ctxKey struct{}

// AppFrom returns the app id bound to a custom-domain request.
func AppFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKey{}).(string)
	return v, ok
}

// customerPrefixes are the only routes served on app custom domains.
var customerPrefixes = []string{"/pay/", "/return/", "/i/", "/s/", "/assets/", "/theme/", "/l/", "/.well-known/paymux/"}

// IsCustomerRoute reports whether path may be served on a custom domain.
func IsCustomerRoute(path string) bool {
	for _, p := range customerPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Normalize lower-cases a host and strips any port.
func Normalize(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func servable(d Domain) bool { return d.Status == StatusActive || d.Status == StatusPending }

// Router dispatches by Host.
type Router struct {
	BaseHost string
	Lookup   Lookup
	Base     http.Handler // everything on the base host
	Customer http.Handler // customer routes on custom hosts (app id in context)
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := Normalize(r.Host)
	if host == Normalize(rt.BaseHost) {
		rt.Base.ServeHTTP(w, r)
		return
	}
	d, ok, err := rt.Lookup.LookupHost(r.Context(), host)
	if err != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ok || !servable(d) {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	if !IsCustomerRoute(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	rt.Customer.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, d.AppID)))
}

// TLSAllowOptions configures the handler for Caddy's on-demand TLS "ask".
type TLSAllowOptions struct {
	BaseHost string
	Lookup   Lookup
	// Resolve returns the addresses a host name resolves to; nil uses the
	// system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Timeout bounds the DNS lookups of one request (default 3 seconds).
	Timeout time.Duration
}

// TLSAllow answers Caddy's on_demand_tls ask: 200 when a certificate may be
// issued for the domain, 403 when not. Mount it on the internal listener only.
//
// Certificates are a limited resource (the CA caps how many it issues), and a
// domain is added in the dashboard by anyone with the right, so the answer is
// 200 only for
//
//   - the base host,
//   - a domain whose ownership was verified (active), and
//   - a domain still pending verification only while its A or AAAA records
//     already lead to the hub, that is, include one of the base host's.
//
// Everything else, including a pending domain whose DNS cannot be read, is 403.
type TLSAllow struct{ o TLSAllowOptions }

// NewTLSAllow builds the handler.
func NewTLSAllow(o TLSAllowOptions) *TLSAllow {
	if o.Resolve == nil {
		o.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Second
	}
	return &TLSAllow{o: o}
}

// TLSAllowHandler is NewTLSAllow with the system resolver.
func TLSAllowHandler(baseHost string, lookup Lookup) http.Handler {
	return NewTLSAllow(TLSAllowOptions{BaseHost: baseHost, Lookup: lookup})
}

func (h *TLSAllow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := Normalize(r.URL.Query().Get("domain"))
	switch {
	case host == "":
		http.Error(w, "domain required", http.StatusBadRequest)
		return
	case host == Normalize(h.o.BaseHost):
		w.WriteHeader(http.StatusOK)
		return
	}
	d, ok, err := h.o.Lookup.LookupHost(r.Context(), host)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusServiceUnavailable)
		return
	}
	switch {
	case !ok:
	case d.Status == StatusActive:
		w.WriteHeader(http.StatusOK)
		return
	case d.Status == StatusPending && h.pointsHere(r.Context(), host):
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "not allowed", http.StatusForbidden)
}

// pointsHere reports whether host resolves to at least one address the base
// host resolves to.
func (h *TLSAllow) pointsHere(ctx context.Context, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, h.o.Timeout)
	defer cancel()
	hub, err := h.o.Resolve(ctx, Normalize(h.o.BaseHost))
	if err != nil || len(hub) == 0 {
		return false
	}
	theirs, err := h.o.Resolve(ctx, host)
	if err != nil {
		return false
	}
	for _, a := range theirs {
		for _, b := range hub {
			if a.Unmap() == b.Unmap() {
				return true
			}
		}
	}
	return false
}
