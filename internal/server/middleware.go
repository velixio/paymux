package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/logctx"
	"github.com/paymux/paymux/internal/metrics"
)

const requestTimeout = 30 * time.Second

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyRoute
)

// ResponseHook is told about every finished request: its method, the matched
// route pattern ("unmatched" when none), the status and how long it took. The
// context carries the request's identifiers (package logctx).
type ResponseHook func(ctx context.Context, method, route string, status int, d time.Duration)

// RequestID returns the request ID stored by the middleware, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(keyRequestID).(string)
	return id
}

// routeHolder lets the matched mux pattern travel back out through handlers
// that copy the request (such as TimeoutHandler).
type routeHolder struct{ pattern string }

// statusWriter records the response status.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	// tagID is set when a plain-text 5xx page is being written: the request id
	// is added at its end.
	tagID bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
		h := w.Header()
		w.tagID = code >= 500 && strings.HasPrefix(h.Get("Content-Type"), "text/plain") && h.Get("Content-Length") == ""
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// chain applies, outermost first: request ID, access log, recover, timeout.
func chain(h http.Handler, log *slog.Logger, requests *metrics.CounterVec, onResponse ResponseHook) http.Handler {
	h = http.TimeoutHandler(h, requestTimeout, `{"error":{"type":"timeout"}}`)
	h = recoverer(h, log)
	h = accessLog(h, log, requests, onResponse)
	return requestID(h)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !requestIDPattern.MatchString(id) {
			id = ids.New("req")
		}
		w.Header().Set("X-Request-Id", id)
		// A scope for the identifiers this request learns (org, app, payment, ...),
		// so that a log row or a failed provider call made while serving it says
		// which request it belongs to.
		ctx := logctx.With(logctx.Begin(r.Context()), "request_id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, keyRequestID, id)))
	})
}

func accessLog(next http.Handler, log *slog.Logger, requests *metrics.CounterVec, onResponse ResponseHook) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		holder := &routeHolder{}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), keyRoute, holder)))
		if sw.tagID {
			// A plain-text error page ("internal error"): say which request failed,
			// so a person can quote it and an operator can find the log row.
			_, _ = fmt.Fprintf(sw.ResponseWriter, "request id: %s\n", RequestID(r.Context()))
		}
		route := holder.pattern
		if route == "" {
			route = "unmatched"
		}
		requests.Inc(route, itoa(sw.status))
		if onResponse != nil {
			onResponse(r.Context(), r.Method, route, sw.status, time.Since(start))
		}
		log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("request_id", RequestID(r.Context())),
			slog.String("method", r.Method),
			slog.String("route", route),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

func recoverer(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil || rec == http.ErrAbortHandler {
				if rec != nil {
					panic(rec)
				}
				return
			}
			id := RequestID(r.Context())
			log.Error("panic in handler", "request_id", id, "panic", rec, "stack", string(debug.Stack()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"type": "internal", "request_id": id},
			})
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
