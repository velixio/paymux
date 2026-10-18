package dblog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/paymux/paymux/internal/store"
)

// OTLPOptions configures the optional export of the stored rows to an
// OpenTelemetry collector (PAYMUX_OTLP_LOGS_ENDPOINT).
type OTLPOptions struct {
	Endpoint string            // the collector's logs URL, e.g. http://collector:4318/v1/logs
	Headers  map[string]string // sent with every request (an auth token, a tenant)
	Service  string            // service.name; default "paymux"
	Version  string            // service.version
	// Client sends the requests; the default has a 10 second timeout and, on
	// purpose, no address guard: the endpoint is set by the operator, who may
	// point it at a private collector.
	Client     *http.Client
	QueueSize  int           // batches waiting to be sent; default 64
	FlushEvery time.Duration // default 2s
	// Stdout receives the shipper's own messages. It must not tee into the Recorder.
	Stdout *slog.Logger
}

// OTLP ships rows over OTLP/HTTP with a JSON payload. It hand-rolls the small
// logs payload instead of pulling in the OpenTelemetry SDK. The rows it gets
// are the ones already stored, so they are redacted. It never blocks the
// caller: a full queue or a down collector costs the batch, which is counted.
type OTLP struct {
	o       OTLPOptions
	ch      chan []store.LogRow
	quit    chan struct{}
	done    chan struct{}
	started sync.Once
	closed  atomic.Bool
	dropped atomic.Uint64
	failed  atomic.Uint64
	lastErr atomic.Int64
}

// NewOTLP builds a shipper. Call Start to begin sending.
func NewOTLP(o OTLPOptions) *OTLP {
	if o.Service == "" {
		o.Service = "paymux"
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 64
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = 2 * time.Second
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Stdout == nil {
		o.Stdout = slog.New(slog.DiscardHandler)
	}
	return &OTLP{o: o, ch: make(chan []store.LogRow, o.QueueSize), quit: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the sender goroutine (once).
func (s *OTLP) Start() { s.started.Do(func() { go s.run() }) }

// Close sends what is queued, then stops. Rows shipped afterwards are ignored.
func (s *OTLP) Close() {
	if s.closed.Swap(true) {
		return
	}
	close(s.quit)
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
}

// Dropped is the number of batches lost because the queue was full.
func (s *OTLP) Dropped() uint64 { return s.dropped.Load() }

// Ship queues rows for sending. It never blocks.
func (s *OTLP) Ship(rows []store.LogRow) {
	if len(rows) == 0 || s.closed.Load() {
		return
	}
	cp := append([]store.LogRow(nil), rows...)
	select {
	case s.ch <- cp:
	default:
		s.dropped.Add(1)
	}
}

func (s *OTLP) run() {
	defer close(s.done)
	tick := time.NewTicker(s.o.FlushEvery)
	defer tick.Stop()
	var batch []store.LogRow
	send := func() {
		if len(batch) > 0 {
			s.send(batch)
			batch = nil
		}
	}
	for {
		select {
		case rows := <-s.ch:
			batch = append(batch, rows...)
			if len(batch) >= 200 {
				send()
			}
		case <-tick.C:
			send()
		case <-s.quit:
			for {
				select {
				case rows := <-s.ch:
					batch = append(batch, rows...)
				default:
					send()
					return
				}
			}
		}
	}
}

func (s *OTLP) send(rows []store.LogRow) {
	body, err := json.Marshal(s.payload(rows))
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.o.Endpoint, bytes.NewReader(body))
	if err != nil {
		s.fail(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.o.Headers {
		req.Header.Set(k, v)
	}
	resp, err := s.o.Client.Do(req)
	if err != nil {
		s.fail(err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.fail(&statusError{resp.StatusCode})
	}
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "the collector answered " + strconv.Itoa(e.code) }

// fail counts a lost batch and reports it on stdout at most once a minute.
func (s *OTLP) fail(err error) {
	s.failed.Add(1)
	now := time.Now().UnixNano()
	if last := s.lastErr.Load(); now-last > int64(time.Minute) && s.lastErr.CompareAndSwap(last, now) {
		s.o.Stdout.Warn("could not ship logs over OTLP", "endpoint", s.o.Endpoint, "failed_batches", s.failed.Load(), "err", err.Error())
	}
}

type otlpValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
}

type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{Key: k, Value: otlpValue{StringValue: &v}} }
func intAttr(k string, n int64) otlpAttr {
	v := strconv.FormatInt(n, 10)
	return otlpAttr{Key: k, Value: otlpValue{IntValue: &v}}
}

type otlpRecord struct {
	TimeUnixNano   string     `json:"timeUnixNano"`
	ObservedNano   string     `json:"observedTimeUnixNano"`
	SeverityNumber int        `json:"severityNumber"`
	SeverityText   string     `json:"severityText"`
	Body           otlpValue  `json:"body"`
	Attributes     []otlpAttr `json:"attributes"`
}

func severity(level string) (int, string) {
	switch level {
	case "error":
		return 17, "ERROR"
	case "warn":
		return 13, "WARN"
	case "info":
		return 9, "INFO"
	}
	return 5, "DEBUG"
}

func (s *OTLP) payload(rows []store.LogRow) map[string]any {
	records := make([]otlpRecord, 0, len(rows))
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	for _, r := range rows {
		num, text := severity(r.Level)
		msg := r.Msg
		rec := otlpRecord{TimeUnixNano: strconv.FormatInt(r.At.UnixNano(), 10), ObservedNano: now, SeverityNumber: num, SeverityText: text,
			Body: otlpValue{StringValue: &msg}}
		add := func(k, v string) {
			if v != "" {
				rec.Attributes = append(rec.Attributes, strAttr(k, v))
			}
		}
		add("log.id", r.ID)
		add("kind", r.Kind)
		add("component", r.Component)
		add("request_id", r.RequestID)
		add("job_id", r.JobID)
		add("org_id", r.OrgID)
		add("app_id", r.AppID)
		add("payment_id", r.PaymentID)
		add("attempt_id", r.AttemptID)
		add("refund_id", r.RefundID)
		add("subscription_id", r.SubscriptionID)
		add("provider_account_id", r.ProviderAccountID)
		add("connector", r.Connector)
		if r.IDs != "" {
			var m map[string]string
			if json.Unmarshal([]byte(r.IDs), &m) == nil {
				for k, v := range m {
					add(k, v)
				}
			}
		}
		add("attrs", r.Attrs)
		add("http.request", r.Request)
		add("http.response", r.Response)
		if r.Status != 0 {
			rec.Attributes = append(rec.Attributes, intAttr("http.status_code", int64(r.Status)))
		}
		if r.DurationMS != 0 {
			rec.Attributes = append(rec.Attributes, intAttr("duration_ms", r.DurationMS))
		}
		records = append(records, rec)
	}
	res := []otlpAttr{strAttr("service.name", s.o.Service)}
	if s.o.Version != "" {
		res = append(res, strAttr("service.version", s.o.Version))
	}
	return map[string]any{"resourceLogs": []any{map[string]any{
		"resource":  map[string]any{"attributes": res},
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": "paymux/dblog"}, "logRecords": records}},
	}}}
}
