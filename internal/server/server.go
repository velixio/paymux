// Package server builds the Paymux HTTP handlers: the public mux (health,
// readiness) with its middleware chain, and the internal mux (metrics, pprof).
package server

import (
	"log/slog"
	"net/http"
	"net/http/pprof"

	"github.com/paymux/paymux/internal/metrics"
)

// Options configures a Server.
type Options struct {
	Logger  *slog.Logger
	Metrics *metrics.Registry
	Checks  []Check // evaluated in order by /readyz
}

// Server holds the muxes. Register extra routes before serving.
type Server struct {
	log      *slog.Logger
	reg      *metrics.Registry
	checks   []Check
	mux      *http.ServeMux
	requests *metrics.CounterVec
}

// New builds a Server with /healthz and /readyz registered.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = metrics.NewRegistry()
	}
	s := &Server{
		log:    o.Logger,
		reg:    o.Metrics,
		checks: o.Checks,
		mux:    http.NewServeMux(),
		requests: o.Metrics.Counter("paymux_http_requests_total",
			"HTTP requests by route pattern and status code.", "route", "code"),
	}
	s.HandleFunc("GET /healthz", s.healthz)
	s.HandleFunc("GET /readyz", s.readyz)
	return s
}

// HandleFunc registers a route on the public mux.
func (s *Server) HandleFunc(pattern string, fn http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if h, ok := r.Context().Value(keyRoute).(*routeHolder); ok {
			h.pattern = pattern
		}
		fn(w, r)
	})
}

// Handler is the public handler wrapped in the middleware chain.
func (s *Server) Handler() http.Handler { return chain(s.mux, s.log, s.requests) }

// InternalHandler serves /metrics and /debug/pprof/ for the internal listener.
func (s *Server) InternalHandler() http.Handler {
	m := http.NewServeMux()
	m.Handle("GET /metrics", s.reg.Handler())
	m.HandleFunc("/debug/pprof/", pprof.Index)
	m.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	m.HandleFunc("/debug/pprof/profile", pprof.Profile)
	m.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	m.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return m
}
