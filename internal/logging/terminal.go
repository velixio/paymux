package logging

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
)

// HTTPRequestMsg is the message of the server's access log lines.
const HTTPRequestMsg = "http request"

// Terminal is the JSON logger of a hub's stderr, with a level and a switch for
// request lines that can change while it runs. It starts at info with request
// lines shown. Both only decide what is printed: the database log wraps this
// handler and makes its own choice.
type Terminal struct {
	level        slog.LevelVar
	hideRequests atomic.Bool
	log          *slog.Logger
}

// NewTerminal returns a Terminal writing to w.
func NewTerminal(w io.Writer) *Terminal {
	t := &Terminal{}
	t.level.Set(slog.LevelInfo)
	t.log = slog.New(&terminalHandler{t: t, next: slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       &t.level,
		ReplaceAttr: replaceAttr,
	})})
	return t
}

// Logger returns the logger. It follows later changes.
func (t *Terminal) Logger() *slog.Logger { return t.log }

// SetLevel sets the lowest level printed.
func (t *Terminal) SetLevel(l slog.Level) { t.level.Set(l) }

// SetRequests shows or hides the access log lines of successful requests.
// Failed requests (status 400 and up) keep printing.
func (t *Terminal) SetRequests(show bool) { t.hideRequests.Store(!show) }

type terminalHandler struct {
	t    *Terminal
	next slog.Handler
}

func (h *terminalHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *terminalHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.t.hideRequests.Load() && successfulRequest(rec) {
		return nil
	}
	return h.next.Handle(ctx, rec)
}

func (h *terminalHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &terminalHandler{t: h.t, next: h.next.WithAttrs(as)}
}

func (h *terminalHandler) WithGroup(name string) slog.Handler {
	return &terminalHandler{t: h.t, next: h.next.WithGroup(name)}
}

func successfulRequest(rec slog.Record) bool {
	if rec.Message != HTTPRequestMsg {
		return false
	}
	ok := false
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != "status" {
			return true
		}
		v := a.Value.Resolve()
		ok = v.Kind() == slog.KindInt64 && v.Int64() < http.StatusBadRequest
		return false
	})
	return ok
}
