package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"
)

type shutdownDeadlineWriter struct {
	*httptest.ResponseRecorder
	writeStarted, deadlineSet chan struct{}
	deadline                  time.Time
	writeReturned             bool
}

func (w *shutdownDeadlineWriter) Write([]byte) (int, error) {
	close(w.writeStarted)
	<-w.deadlineSet
	w.writeReturned = true
	return 0, context.DeadlineExceeded
}

func (w *shutdownDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	close(w.deadlineSet)
	return nil
}

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

// TestApplicationShutdownCancelsOnlyTheDashboardEventStream verifies that
// shutdown interrupts a blocked SSE write without canceling ordinary requests.
func TestApplicationShutdownCancelsOnlyTheDashboardEventStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		appCtx, stopApp := context.WithCancel(context.Background())
		serveDone := make(chan struct{})
		w := &shutdownDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), writeStarted: make(chan struct{}), deadlineSet: make(chan struct{})}
		var requestErr, writeErr error
		h := cancelDashboardStreamOnShutdown(appCtx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, writeErr = w.Write([]byte("data: blocked\n\n"))
			requestErr = r.Context().Err()
		}))
		go func() {
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard/events", nil))
			close(serveDone)
		}()

		synctest.Wait()
		select {
		case <-w.writeStarted:
		default:
			t.Fatal("dashboard event stream did not start its write")
		}
		select {
		case <-serveDone:
			t.Fatal("blocked dashboard event write returned before shutdown")
		default:
		}

		stopApp()
		synctest.Wait()
		select {
		case <-serveDone:
		default:
			t.Fatal("dashboard event stream did not return after shutdown")
		}
		if !w.writeReturned || writeErr != context.DeadlineExceeded || requestErr != context.Canceled || w.deadline.IsZero() {
			t.Fatalf("stream shutdown: write_returned=%v write_err=%v request_err=%v deadline=%v",
				w.writeReturned, writeErr, requestErr, w.deadline)
		}

		var ordinaryErr error
		cancelDashboardStreamOnShutdown(appCtx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			ordinaryErr = r.Context().Err()
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/payments", nil))
		if ordinaryErr != nil {
			t.Fatalf("ordinary request context was canceled during graceful drain: %v", ordinaryErr)
		}
	})
}
