package dblog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/redact"
	"github.com/velixio/paymux/internal/store"
)

// idAliases maps a log attribute name to the identifier it carries. Anything
// not listed here is an ordinary attribute, except "<x>_id" attributes whose
// value looks like one of our ids.
var idAliases = map[string]string{
	"request_id": "request_id", "req_id": "request_id",
	"job_id": "job_id", "job_kind": "job_kind",
	"org_id": "org_id", "app_id": "app_id",
	"payment_id": "payment_id", "attempt_id": "attempt_id", "refund_id": "refund_id",
	"subscription_id": "subscription_id", "sub_id": "subscription_id",
	"mandate_id": "mandate_id", "payout_id": "payout_id", "invoice_id": "invoice_id",
	"customer_id": "customer_id", "dispute_id": "dispute_id", "settlement_id": "settlement_id", "link_id": "link_id",
	"endpoint_id": "endpoint_id", "delivery_id": "delivery_id", "user_id": "user_id",
	"provider_account": "provider_account_id", "provider_account_id": "provider_account_id", "account": "provider_account_id", "connector": "connector",
	"provider_event_id": "provider_event_id",
	"provider_order_id": "provider_order_id", "provider_payment_id": "provider_payment_id", "provider_refund_id": "provider_refund_id",
	"provider_request_id": "provider_request_id", "provider_ref": "provider_ref",
	"event_id": "webhook_event_id", "webhook_event_id": "webhook_event_id",
}

var (
	reIDValue = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,128}$`)
	reOurID   = regexp.MustCompile(`^[a-z]{2,5}_[0-9a-z]{26}$`)
)

type kvAttr struct {
	key string
	val slog.Value
}

type handler struct {
	r      *Recorder
	next   slog.Handler
	attrs  []kvAttr // from With, groups already applied
	groups []string
}

// Handler returns a slog.Handler that passes every record to next with the
// known secret values (provider credentials, the master key) replaced, and
// also stores the records at or above the recorder's level, fully redacted. A
// record that cannot be stored is simply not stored.
func (r *Recorder) Handler(next slog.Handler) slog.Handler { return &handler{r: r, next: next} }

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l) || l >= h.r.Level()
}

func (h *handler) Handle(ctx context.Context, rec slog.Record) error {
	var err error
	if h.next.Enabled(ctx, rec.Level) {
		err = h.next.Handle(ctx, h.scrubbed(rec))
	}
	if rec.Level >= h.r.Level() {
		func() {
			defer func() { _ = recover() }() // a hostile attribute must not break logging
			h.r.Emit(h.row(ctx, rec))
		}()
	}
	return err
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(scrubAttrs(h.r.red, as))
	c.attrs = append(append([]kvAttr(nil), h.attrs...), flatten(h.groups, as)...)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

// scrubbed is rec with the known secret values replaced, for the next handler
// (stderr): its own rules cannot know which values are this hub's secrets.
func (h *handler) scrubbed(rec slog.Record) slog.Record {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.red.Scrub(rec.Message), rec.PC)
	var as []slog.Attr
	rec.Attrs(func(a slog.Attr) bool {
		as = append(as, a)
		return true
	})
	out.AddAttrs(scrubAttrs(h.r.red, as)...)
	return out
}

func scrubAttrs(red *redact.Redactor, as []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		v := a.Value.Resolve()
		switch v.Kind() {
		case slog.KindGroup:
			out[i] = slog.Attr{Key: a.Key, Value: slog.GroupValue(scrubAttrs(red, v.Group())...)}
		case slog.KindString:
			out[i] = slog.String(a.Key, red.Scrub(v.String()))
		case slog.KindAny:
			var s string
			if err, ok := v.Any().(error); ok && err != nil {
				s = err.Error()
			} else if v.Any() != nil {
				s = fmt.Sprint(v.Any())
			}
			if t := red.Scrub(s); t != s {
				out[i] = slog.String(a.Key, t)
			} else {
				out[i] = slog.Attr{Key: a.Key, Value: v}
			}
		default:
			out[i] = slog.Attr{Key: a.Key, Value: v}
		}
	}
	return out
}

// flatten turns attributes into dotted key / value pairs.
func flatten(groups []string, as []slog.Attr) []kvAttr {
	var out []kvAttr
	for _, a := range as {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			g := groups
			if a.Key != "" {
				g = append(append([]string(nil), groups...), a.Key)
			}
			out = append(out, flatten(g, v.Group())...)
			continue
		}
		if a.Key == "" {
			continue
		}
		out = append(out, kvAttr{key: strings.Join(append(append([]string(nil), groups...), a.Key), "."), val: v})
	}
	return out
}

func leaf(key string) string {
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		return key[i+1:]
	}
	return key
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

// component names the package a record was logged from: "internal/jobs".
func component(pc uintptr) string {
	if pc == 0 {
		return "app"
	}
	fr, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	fn := fr.Function
	fn = strings.TrimPrefix(fn, "github.com/velixio/paymux/")
	if i := strings.LastIndexByte(fn, '/'); i >= 0 {
		if j := strings.IndexByte(fn[i:], '.'); j >= 0 {
			return fn[:i+j]
		}
		return fn
	}
	if j := strings.IndexByte(fn, '.'); j > 0 {
		return fn[:j]
	}
	return "app"
}

func (h *handler) row(ctx context.Context, rec slog.Record) store.LogRow {
	red := h.r.red
	idm := logctx.IDs(ctx)
	attrs := map[string]any{}
	all := append([]kvAttr(nil), h.attrs...)
	rec.Attrs(func(a slog.Attr) bool {
		all = append(all, flatten(h.groups, []slog.Attr{a})...)
		return true
	})
	kind, comp := "", ""
	for _, a := range all {
		l := strings.ToLower(leaf(a.key))
		switch {
		case l == "kind" && a.val.Kind() == slog.KindString && validKind(a.val.String()):
			kind = a.val.String()
		case l == "component" && a.val.Kind() == slog.KindString:
			comp = red.Text(a.val.String())
		case isIDAttr(l, a.val):
			idm[idName(l)] = red.Text(a.val.String())
		default:
			attrs[a.key] = attrValue(red, a.key, a.val)
		}
	}
	if comp == "" {
		comp = component(rec.PC)
	}
	if kind == "" {
		kind = "app"
		if idm["job_id"] != "" {
			kind = "job"
		}
	}
	at := rec.Time
	if at.IsZero() {
		at = time.Now()
	}
	row := store.LogRow{ID: ids.New("log"), At: at, Level: levelName(rec.Level), Kind: kind, Component: capStr(comp, 120),
		Msg: capStr(red.Text(rec.Message), 2000)}
	if len(attrs) > 0 {
		row.Attrs = marshalCapped(attrs)
	}
	fillIDs(&row, idm)
	return row
}

func validKind(k string) bool { return k == "app" || k == "provider" || k == "http" || k == "job" }

func idName(l string) string {
	if n, ok := idAliases[l]; ok {
		return n
	}
	return l
}

// isIDAttr says whether an attribute carries an identifier: a known name with
// an id-shaped value, or any "<x>_id" whose value is one of our own ids.
func isIDAttr(l string, v slog.Value) bool {
	if v.Kind() != slog.KindString {
		return false
	}
	s := v.String()
	if _, ok := idAliases[l]; ok {
		return reIDValue.MatchString(s)
	}
	return strings.HasSuffix(l, "_id") && reOurID.MatchString(s)
}

func attrValue(red *redact.Redactor, key string, v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return capStr(valueString(red, key, v.String()), 2000)
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339)
	}
	out := red.Value(key, v.Any())
	if s, ok := out.(string); ok {
		return capStr(s, 2000)
	}
	return out
}

func valueString(red *redact.Redactor, key, s string) string {
	if out, ok := red.Value(key, s).(string); ok {
		return out
	}
	return s
}

func capStr(s string, n int) string { return redact.Cap(s, n) }

func marshalCapped(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	if len(b) > 16<<10 {
		b, _ = json.Marshal(map[string]any{"truncated": true, "keys": len(m)})
	}
	return string(b)
}

// fillIDs puts the indexed identifiers into their columns and the rest into
// the ids JSON.
func fillIDs(row *store.LogRow, idm map[string]string) {
	other := map[string]string{}
	for k, v := range idm {
		if v == "" || !reIDValue.MatchString(v) {
			continue
		}
		switch k {
		case "request_id":
			row.RequestID = v
		case "job_id":
			row.JobID = v
		case "org_id":
			row.OrgID = v
		case "app_id":
			row.AppID = v
		case "payment_id":
			row.PaymentID = v
		case "attempt_id":
			row.AttemptID = v
		case "refund_id":
			row.RefundID = v
		case "subscription_id":
			row.SubscriptionID = v
		case "provider_account_id":
			row.ProviderAccountID = v
		case "connector":
			row.Connector = v
		default:
			other[k] = v
		}
	}
	if len(other) > 0 {
		if b, err := json.Marshal(other); err == nil {
			row.IDs = string(b)
		}
	}
}
