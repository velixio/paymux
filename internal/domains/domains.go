// Package domains routes requests by Host: the base host serves everything,
// app custom domains serve only customer-facing routes (§6.2), unknown hosts
// get 421. It also answers Caddy's on-demand TLS "ask" requests.
package domains

import (
	"context"
	"net"
	"net/http"
	"strings"
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
var customerPrefixes = []string{"/pay/", "/return/", "/i/", "/s/", "/assets/", "/.well-known/paymux/"}

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

// TLSAllowHandler answers Caddy on_demand_tls ask: 200 for the base host and
// pending/active custom domains, 403 otherwise. Mount on the internal listener only.
func TLSAllowHandler(baseHost string, lookup Lookup) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := Normalize(r.URL.Query().Get("domain"))
		switch {
		case host == "":
			http.Error(w, "domain required", http.StatusBadRequest)
			return
		case host == Normalize(baseHost):
			w.WriteHeader(http.StatusOK)
			return
		}
		d, ok, err := lookup.LookupHost(r.Context(), host)
		if err != nil {
			http.Error(w, "lookup failed", http.StatusServiceUnavailable)
			return
		}
		if !ok || !servable(d) {
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
