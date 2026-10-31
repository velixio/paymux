package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/metrics"
)

// requestTimeout bounds every route but the exports (a variable for the tests).
var requestTimeout = 30 * time.Second

// exportTimeout bounds an export: the context of its request and the write
// deadline of its connection.
const exportTimeout = 5 * time.Minute

// streamed are routes whose output must reach the client incrementally.
// http.TimeoutHandler buffers them and strips streaming support, so they run
// outside it (audit H-15).
var streamed = map[string]bool{
	"/dashboard/events":       true,
	"/dashboard/logs.jsonl":   true,
	"/dashboard/reports/zip":  true,
	"/dashboard/reports/file": true,
}

// IsStreamed reports whether a request to path is served outside the timeout
// handler, as a stream under its own deadline.
func IsStreamed(method, path string) bool { return method == http.MethodGet && streamed[path] }

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyRoute
	keySource
)

// Source is the client address of the request being served, as the server's
// ClientIP function names it (the connection's address by default). It is for
// bounding what one sender can make the hub do or record; it is not stored.
func Source(ctx context.Context) string {
	s, _ := ctx.Value(keySource).(string)
	return s
}

// sourceOf names the sender: the ClientIP function's answer, or the address of
// the connection.
func sourceOf(r *http.Request, clientIP func(*http.Request) string) string {
	if clientIP != nil {
		return clientIP(r)
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

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
//
// A handler that outlives its TimeoutHandler may still set the pattern while
// the access log reads it, so both go through the mutex.
type routeHolder struct {
	mu      sync.Mutex
	pattern string
}

func (h *routeHolder) set(p string) { h.mu.Lock(); h.pattern = p; h.mu.Unlock() }

func (h *routeHolder) get() string { h.mu.Lock(); defer h.mu.Unlock(); return h.pattern }

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

// chain applies, outermost first: request ID, access log, recover, timeout
// (except for the streamed export routes, which get a deadline of their own).
func chain(h http.Handler, log *slog.Logger, requests *metrics.CounterVec, onResponse ResponseHook, clientIP func(*http.Request) string) http.Handler {
	inner := h
	timed := http.TimeoutHandler(inner, requestTimeout, `{"error":{"type":"timeout"}}`)
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsStreamed(r.Method, r.URL.Path) {
			timed.ServeHTTP(w, r)
			return
		}
		// Straight to the connection (so the output streams), with a deadline of
		// its own in place of the server's. The wrappers above unwrap for it.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(exportTimeout))
		ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
		defer cancel()
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
	h = recoverer(h, log)
	h = accessLog(h, log, requests, onResponse, clientIP)
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

func accessLog(next http.Handler, log *slog.Logger, requests *metrics.CounterVec, onResponse ResponseHook, clientIP func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r = r.WithContext(context.WithValue(r.Context(), keySource, sourceOf(r, clientIP)))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		holder := &routeHolder{}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), keyRoute, holder)))
		if sw.tagID {
			// A plain-text error page ("internal error"): say which request failed,
			// so a person can quote it and an operator can find the log row.
			_, _ = fmt.Fprintf(sw.ResponseWriter, "request id: %s\n", RequestID(r.Context()))
		}
		route := holder.get()
		if route == "" {
			route = "unmatched"
		}
		requests.Inc(route, itoa(sw.status))
		if onResponse != nil {
			onResponse(r.Context(), r.Method, route, sw.status, time.Since(start))
		}
		// Successful static files are high-volume browser plumbing. Keep them in
		// request metrics and response recording, but leave routine asset and
		// theme traffic out of the application log. Errors remain visible.
		if routineStaticRequest(r, sw.status) {
			return
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

func routineStaticRequest(r *http.Request, status int) bool {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || status >= http.StatusBadRequest {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/assets/") || strings.HasPrefix(r.URL.Path, "/theme/")
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
