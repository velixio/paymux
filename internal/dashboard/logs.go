package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/store"
)

// The Logs page: the errors and warnings kept in the database (application
// records, failed provider calls with their redacted bodies, rejected and
// failing requests, failed jobs). Everything shown was redacted before it was
// stored and is redacted again on the way out. Only logs.read may see it.

const (
	logExportMax   = 10000
	logExportChunk = 500
)

var (
	logLevels = []string{"error", "warn", "info", "debug"}
	logKinds  = []string{"app", "provider", "http", "job"}
)

type logFilter struct {
	Level, Kind, Connector, Account, RequestID, PaymentID, ID, Q, From, To string
}

type logChip struct{ Label, Href string }

type logRowView struct {
	ID, At, Level, Kind, Component, Msg string
	Status, Duration                    string
	Chips                               []logChip
}

type logDetail struct {
	logRowView
	Facts                []logFact
	Attrs, Request, Resp string
}

type logFact struct {
	Name, Value, Href string
}

func (d *Dashboard) routesLogs() {
	m := d.mux
	m.HandleFunc("GET "+prefix+"/logs", d.authed(authz.LogsRead, d.logsPage))
	m.HandleFunc("GET "+prefix+"/logs.jsonl", d.authed(authz.LogsRead, d.logsExport))
	m.HandleFunc("GET "+prefix+"/logs/{id}", d.authed(authz.LogsRead, d.logPage))
}

// logsURL is the Logs page filtered by one id.
func logsURL(param, value string) string {
	return prefix + "/logs?" + url.Values{param: {value}}.Encode()
}

// logOut redacts a stored text again with the redactor Paymux runs with, so a
// secret that became known after the row was stored is still hidden.
func (d *Dashboard) logOut(s string) string {
	if d.o.LogRedact == nil || s == "" {
		return s
	}
	return d.o.LogRedact(s)
}

func parseLogFilter(r *http.Request) logFilter {
	q := r.URL.Query()
	t := func(k string) string { return strings.TrimSpace(q.Get(k)) }
	return logFilter{Level: t("level"), Kind: t("kind"), Connector: t("connector"), Account: t("account"), RequestID: t("request_id"),
		PaymentID: t("payment_id"), ID: t("id"), Q: t("q"), From: t("from"), To: t("to")}
}

// storeFilter validates f and converts it to a store filter.
func (d *Dashboard) storeFilter(f logFilter) (store.LogFilter, string) {
	sf := store.LogFilter{Level: f.Level, Kind: f.Kind, Connector: f.Connector, ProviderAccountID: f.Account, RequestID: f.RequestID,
		PaymentID: f.PaymentID, AnyID: f.ID, Text: f.Q}
	if f.Level != "" && !slices.Contains(logLevels, f.Level) {
		return sf, "unknown level"
	}
	if f.Kind != "" && !slices.Contains(logKinds, f.Kind) {
		return sf, "unknown kind"
	}
	if f.From != "" {
		t, err := d.parseDay(f.From)
		if err != nil {
			return sf, "from must be a date like 2026-10-01"
		}
		sf.From = t
	}
	if f.To != "" {
		t, err := d.parseDay(f.To)
		if err != nil {
			return sf, "to must be a date like 2026-10-01"
		}
		sf.To = t.AddDate(0, 0, 1)
	}
	return sf, ""
}

func (f logFilter) values() url.Values {
	v := url.Values{}
	for k, val := range map[string]string{"level": f.Level, "kind": f.Kind, "connector": f.Connector, "account": f.Account,
		"request_id": f.RequestID, "payment_id": f.PaymentID, "id": f.ID, "q": f.Q, "from": f.From, "to": f.To} {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v
}

// cursor is "<unix ms>.<id>": rows older than that row.
func parseCursor(s string) (time.Time, string, bool) {
	ms, id, ok := strings.Cut(s, ".")
	n, err := strconv.ParseInt(ms, 10, 64)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", false
	}
	return time.UnixMilli(n).UTC(), id, true
}

func (d *Dashboard) logsDisabled() bool { return d.o.LogDB == nil }

func (d *Dashboard) logsPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	f := parseLogFilter(r)
	sf, bad := d.storeFilter(f)
	if bad != "" {
		badRequest(w, bad)
		return
	}
	if before := r.URL.Query().Get("before"); before != "" {
		at, id, ok := parseCursor(before)
		if !ok {
			badRequest(w, "bad cursor")
			return
		}
		sf.BeforeAt, sf.BeforeID = at, id
	}
	v := d.shell(r, s, "logs", "Logs")
	v.LogFilter, v.LogLevels, v.LogKinds = f, logLevels, logKinds
	v.LogExportURL = prefix + "/logs.jsonl"
	if q := f.values().Encode(); q != "" {
		v.LogExportURL += "?" + q
	}
	if d.logsDisabled() {
		v.Error = "The error log in the database is not available on this instance."
		d.render(w, http.StatusOK, "dash_logs", v)
		return
	}
	sf.Limit = d.o.PageSize + 1
	var rows []store.LogRow
	if err := d.o.LogDB.Read(ctx, func(q store.Querier) (err error) { rows, err = store.SearchLogs(ctx, q, sf); return }); err != nil {
		d.fail(w, err)
		return
	}
	more := len(rows) > d.o.PageSize
	if more {
		rows = rows[:d.o.PageSize]
	}
	for _, row := range rows {
		v.LogRows = append(v.LogRows, d.viewLog(row))
	}
	if more {
		last := rows[len(rows)-1]
		next := f.values()
		next.Set("before", fmt.Sprintf("%d.%s", last.At.UnixMilli(), last.ID))
		v.NextURL = prefix + "/logs?" + next.Encode()
	}
	d.render(w, http.StatusOK, "dash_logs", v)
}

func (d *Dashboard) viewLog(r store.LogRow) logRowView {
	v := logRowView{ID: r.ID, At: d.fmtTime(r.At), Level: r.Level, Kind: r.Kind, Component: r.Component, Msg: d.logOut(r.Msg)}
	if r.Status != 0 {
		v.Status = strconv.Itoa(r.Status)
	}
	if r.DurationMS != 0 {
		v.Duration = (time.Duration(r.DurationMS) * time.Millisecond).String()
	}
	for _, c := range []struct{ label, param, val string }{{"request", "request_id", r.RequestID}, {"payment", "payment_id", r.PaymentID},
		{"attempt", "id", r.AttemptID}, {"refund", "id", r.RefundID}, {"subscription", "id", r.SubscriptionID}, {"job", "id", r.JobID},
		{"account", "account", r.ProviderAccountID}} {
		if c.val != "" {
			v.Chips = append(v.Chips, logChip{Label: c.label + " " + d.logOut(c.val), Href: logsURL(c.param, c.val)})
		}
	}
	return v
}

func (d *Dashboard) logPage(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	if d.logsDisabled() {
		http.NotFound(w, r)
		return
	}
	var row store.LogRow
	err := d.o.LogDB.Read(ctx, func(q store.Querier) (err error) { row, err = store.GetLog(ctx, q, r.PathValue("id")); return })
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		d.fail(w, err)
		return
	}
	det := &logDetail{logRowView: d.viewLog(row)}
	add := func(name, value, param string) {
		if value != "" {
			f := logFact{Name: name, Value: d.logOut(value)}
			if param != "" {
				f.Href = logsURL(param, value)
			}
			det.Facts = append(det.Facts, f)
		}
	}
	add("Request id", row.RequestID, "request_id")
	add("Job id", row.JobID, "id")
	add("Org", row.OrgID, "id")
	add("App", row.AppID, "id")
	add("Payment", row.PaymentID, "payment_id")
	add("Attempt", row.AttemptID, "id")
	add("Refund", row.RefundID, "id")
	add("Subscription", row.SubscriptionID, "id")
	add("Provider account", row.ProviderAccountID, "account")
	add("Connector", row.Connector, "connector")
	if row.IDs != "" {
		var m map[string]string
		if json.Unmarshal([]byte(row.IDs), &m) == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(strings.ReplaceAll(k, "_", " "), m[k], "id")
			}
		}
	}
	det.Attrs, det.Request, det.Resp = d.prettyLog(row.Attrs), d.prettyLog(row.Request), d.prettyLog(row.Response)
	v := d.shell(r, s, "logs", "Log "+row.ID)
	v.LogEntry = det
	d.render(w, http.StatusOK, "dash_log", v)
}

// prettyLog indents a stored JSON document, redacting it again first.
func (d *Dashboard) prettyLog(raw string) string {
	raw = d.logOut(raw)
	if raw == "" {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, []byte(raw), "", "  ") != nil {
		return raw
	}
	return buf.String()
}

// logLine is one line of the JSON-lines export.
type logLine struct {
	ID                string          `json:"id"`
	Time              string          `json:"time"`
	Level             string          `json:"level"`
	Kind              string          `json:"kind"`
	Component         string          `json:"component"`
	Msg               string          `json:"msg"`
	RequestID         string          `json:"request_id,omitempty"`
	JobID             string          `json:"job_id,omitempty"`
	OrgID             string          `json:"org_id,omitempty"`
	AppID             string          `json:"app_id,omitempty"`
	PaymentID         string          `json:"payment_id,omitempty"`
	AttemptID         string          `json:"attempt_id,omitempty"`
	RefundID          string          `json:"refund_id,omitempty"`
	SubscriptionID    string          `json:"subscription_id,omitempty"`
	ProviderAccountID string          `json:"provider_account_id,omitempty"`
	Connector         string          `json:"connector,omitempty"`
	IDs               json.RawMessage `json:"ids,omitempty"`
	Attrs             json.RawMessage `json:"attrs,omitempty"`
	Request           json.RawMessage `json:"request,omitempty"`
	Response          json.RawMessage `json:"response,omitempty"`
	Status            int             `json:"status,omitempty"`
	DurationMS        int64           `json:"duration_ms,omitempty"`
}

// rawJSON is s as a JSON value: itself when it is valid JSON, else a string.
func (d *Dashboard) rawJSON(s string) json.RawMessage {
	s = d.logOut(s)
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

func (d *Dashboard) logsExport(w http.ResponseWriter, r *http.Request, s *session) {
	ctx := r.Context()
	f := parseLogFilter(r)
	sf, bad := d.storeFilter(f)
	if bad != "" {
		badRequest(w, bad)
		return
	}
	if d.logsDisabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="paymux-logs-`+d.o.Now().UTC().Format("20060102-150405")+`.jsonl"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	sent := 0
	sf.WithBodies, sf.Limit = true, logExportChunk
	for sent < logExportMax && ctx.Err() == nil {
		var rows []store.LogRow
		if err := d.o.LogDB.Read(ctx, func(q store.Querier) (err error) { rows, err = store.SearchLogs(ctx, q, sf); return }); err != nil {
			d.o.Logger.Error("dashboard: logs export", "err", err)
			return
		}
		for _, row := range rows {
			line := logLine{ID: row.ID, Time: row.At.UTC().Format(time.RFC3339Nano), Level: row.Level, Kind: row.Kind, Component: d.logOut(row.Component),
				Msg: d.logOut(row.Msg), RequestID: row.RequestID, JobID: row.JobID, OrgID: row.OrgID, AppID: row.AppID, PaymentID: row.PaymentID,
				AttemptID: row.AttemptID, RefundID: row.RefundID, SubscriptionID: row.SubscriptionID, ProviderAccountID: row.ProviderAccountID,
				Connector: row.Connector, IDs: d.rawJSON(row.IDs), Attrs: d.rawJSON(row.Attrs), Request: d.rawJSON(row.Request),
				Response: d.rawJSON(row.Response), Status: row.Status, DurationMS: row.DurationMS}
			if err := enc.Encode(line); err != nil {
				return
			}
			sent++
		}
		if len(rows) < logExportChunk {
			return
		}
		last := rows[len(rows)-1]
		sf.BeforeAt, sf.BeforeID = last.At, last.ID
	}
}
