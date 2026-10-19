package dblog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/logctx"
	"github.com/paymux/paymux/internal/redact"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// ProviderMeta says whose calls a provider call recorder reports.
type ProviderMeta struct {
	Connector string
	AccountID string
	OrgID     string
}

// ProviderCall returns the connector.CallRecorder for one provider account.
// A failed attempt becomes an error row with the redacted request and response
// (bodies capped at BodyCap); an attempt the client is about to retry becomes
// a warning without bodies. Successful calls are never reported to it.
func (r *Recorder) ProviderCall(m ProviderMeta) connector.CallRecorder {
	return func(ctx context.Context, c connector.CallRecord) {
		defer func() { _ = recover() }()
		r.Emit(r.providerRow(ctx, m, c))
	}
}

func (r *Recorder) providerRow(ctx context.Context, m ProviderMeta, c connector.CallRecord) store.LogRow {
	red := r.red
	idm := logctx.IDs(ctx)
	if m.Connector != "" {
		idm["connector"] = m.Connector
	}
	if m.AccountID != "" {
		idm["provider_account_id"] = m.AccountID
	}
	if m.OrgID != "" {
		idm["org_id"] = m.OrgID
	}
	resp := c.ResponseHeader
	for k, vs := range resp {
		if lk := strings.ToLower(k); strings.Contains(lk, "request-id") && len(vs) > 0 {
			idm["provider_request_id"] = red.Text(vs[0])
			break
		}
	}
	level := "error"
	if c.WillRetry {
		level = "warn"
	}
	outcome := fmt.Sprintf("%d", c.Status)
	if c.Status == 0 {
		outcome = string(c.Failure)
	}
	u := c.URL
	target := red.Text(u.Host + u.EscapedPath())
	msg := "provider call failed: " + c.Method + " " + target + " -> " + outcome
	if c.WillRetry {
		msg = "provider call failed, retrying: " + c.Method + " " + target + " -> " + outcome
	}
	attrs := map[string]any{"failure": string(c.Failure), "attempt": c.Attempt}
	if c.WillRetry {
		attrs["will_retry"] = true
	}
	if c.Err != nil {
		attrs["error"] = capStr(red.Error(c.Err), 1000)
	}
	row := store.LogRow{ID: ids.New("log"), At: r.o.Now(), Level: level, Kind: "provider", Component: "connector:" + m.Connector,
		Msg: capStr(msg, 2000), Attrs: marshalCapped(attrs), Status: c.Status, DurationMS: c.Duration.Milliseconds()}
	if !c.WillRetry {
		row.Request = r.requestJSON(c)
		row.Response = r.responseJSON(c)
	}
	fillIDs(&row, idm)
	return row
}

type callDoc struct {
	Method    string            `json:"method,omitempty"`
	URL       string            `json:"url,omitempty"`
	Status    int               `json:"status,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      any               `json:"body,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

func (r *Recorder) requestJSON(c connector.CallRecord) string {
	u := c.URL
	d := callDoc{Method: c.Method, URL: r.red.URL(u.String()), Headers: r.red.Headers(c.RequestHeader)}
	d.Body, d.Truncated = r.bodyValue(c.RequestHeader, c.RequestBody, c.RequestTruncated)
	return mustJSON(d)
}

func (r *Recorder) responseJSON(c connector.CallRecord) string {
	if c.Status == 0 && len(c.ResponseBody) == 0 {
		return ""
	}
	d := callDoc{Status: c.Status, Headers: r.red.Headers(c.ResponseHeader)}
	d.Body, d.Truncated = r.bodyValue(c.ResponseHeader, c.ResponseBody, c.ResponseTruncated)
	return mustJSON(d)
}

// body redacts a captured body and caps the redacted text.
func (r *Recorder) body(h http.Header, b []byte, truncated bool) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	if truncated && len(b) > 256 {
		b = b[:len(b)-128] // a secret cut in half at the capture limit is no longer a match: drop the tail
	}
	s := r.red.Body(h.Get("Content-Type"), []byte(strings.ToValidUTF8(string(b), "?")))
	if len(s) > BodyCap {
		s, truncated = redact.Cap(s, BodyCap), true
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	return s, truncated
}

// bodyValue is body, as embedded JSON when the redacted text is a whole JSON
// document, so the Logs page can show it as it is.
func (r *Recorder) bodyValue(h http.Header, b []byte, truncated bool) (any, bool) {
	s, tr := r.body(h, b, truncated)
	if t := strings.TrimSpace(s); !tr && len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t)) {
		return json.RawMessage(t), false
	}
	if s == "" {
		return nil, tr
	}
	return s, tr
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// HTTPEvent is one finished inbound request.
type HTTPEvent struct {
	Method   string
	Route    string // the matched mux pattern, or "unmatched"
	Status   int
	Duration time.Duration
	// Source is who sent the request (its client address). It is used to keep one
	// source from filling the log with rejected requests, and is never stored.
	Source string
}

// ShouldRecordHTTP says whether a response is worth a row: every 5xx, and a
// 4xx on the API and webhook routes (bad signature, bad credentials,
// validation). Ordinary page traffic and anything below 400 is not stored.
func ShouldRecordHTTP(route string, status int) (level string, ok bool) {
	switch {
	case status >= 500:
		return "error", true
	case status >= 400 && isMachineRoute(route):
		return "warn", true
	}
	return "", false
}

func isMachineRoute(route string) bool {
	if i := strings.IndexByte(route, ' '); i >= 0 {
		route = route[i+1:]
	}
	return strings.HasPrefix(route, "/v1/") || strings.HasPrefix(route, "/hooks/")
}

// HTTP records a finished request when ShouldRecordHTTP says so. The request
// id and whatever else the handlers noted (org, app, provider account) come
// from ctx. The route pattern is stored, never the raw path.
func (r *Recorder) HTTP(ctx context.Context, ev HTTPEvent) {
	level, ok := ShouldRecordHTTP(ev.Route, ev.Status)
	if !ok {
		return
	}
	defer func() { _ = recover() }()
	now := r.o.Now()
	allowed, carried := r.throttle.allow(now, ev.Source, ev.Route, ev.Status)
	if !allowed {
		r.suppressed.Add(1)
		return
	}
	route := r.red.Text(ev.Route)
	attrs := map[string]any{"method": ev.Method, "route": route}
	if carried > 0 {
		attrs["suppressed"] = carried // rows not written since the last one for this source and route
	}
	row := store.LogRow{ID: ids.New("log"), At: now, Level: level, Kind: "http", Component: "http",
		Msg:    capStr(fmt.Sprintf("%s %s -> %d", ev.Method, route, ev.Status), 2000),
		Status: ev.Status, DurationMS: ev.Duration.Milliseconds(),
		Attrs: marshalCapped(attrs)}
	fillIDs(&row, logctx.IDs(ctx))
	r.Emit(row)
}
