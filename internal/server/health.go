package server

import (
	"context"
	"net/http"
	"time"
)

// Check is one readiness probe. A nil error means healthy.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

const checkTimeout = 2 * time.Second

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
	defer cancel()
	failing := []string{}
	for _, c := range s.checks {
		if err := c.Fn(ctx); err != nil {
			s.log.Warn("readiness check failed", "check", c.Name, "err", err, "request_id", RequestID(r.Context()))
			failing = append(failing, c.Name)
		}
	}
	if len(failing) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "failing": failing})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
