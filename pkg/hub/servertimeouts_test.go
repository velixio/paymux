package hub

import (
	"net/http"
	"testing"
	"time"
)

// A client that opens a connection and sends a byte a minute (slowloris), or
// reads its response a byte a minute, must not hold a connection for ever.
func TestServersHaveEveryTimeoutAndASizeLimit(t *testing.T) {
	s := newServer(http.NotFoundHandler())
	for name, d := range map[string]time.Duration{
		"ReadHeaderTimeout": s.ReadHeaderTimeout, "ReadTimeout": s.ReadTimeout,
		"WriteTimeout": s.WriteTimeout, "IdleTimeout": s.IdleTimeout,
	} {
		if d <= 0 {
			t.Errorf("%s is not set", name)
		}
		if d > 5*time.Minute {
			t.Errorf("%s = %v is too long", name, d)
		}
	}
	if s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 1<<20 {
		t.Errorf("MaxHeaderBytes = %d", s.MaxHeaderBytes)
	}
}
