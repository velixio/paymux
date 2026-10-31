package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestApplicationShutdownCancelsOnlyTheDashboardEventStream(t *testing.T) {
	appCtx, stopApp := context.WithCancel(context.Background())
	streamStarted := make(chan struct{})
	streamStopped := make(chan struct{})
	h := cancelDashboardStreamOnShutdown(appCtx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(streamStarted)
		<-r.Context().Done()
		close(streamStopped)
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/dashboard/events", nil))
	<-streamStarted
	stopApp()
	select {
	case <-streamStopped:
	case <-time.After(time.Second):
		t.Fatal("dashboard event stream did not stop with the application")
	}

	var ordinaryErr error
	cancelDashboardStreamOnShutdown(appCtx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ordinaryErr = r.Context().Err()
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/payments", nil))
	if ordinaryErr != nil {
		t.Fatalf("ordinary request context was canceled during graceful drain: %v", ordinaryErr)
	}
}
