package dblog_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

type otlpSink struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	status  int
}

func (o *otlpSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	o.mu.Lock()
	o.bodies, o.headers, o.paths = append(o.bodies, b), append(o.headers, r.Header.Clone()), append(o.paths, r.URL.Path)
	st := o.status
	o.mu.Unlock()
	if st != 0 {
		w.WriteHeader(st)
		return
	}
	_, _ = w.Write([]byte(`{}`))
}

func (o *otlpSink) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		got := len(o.bodies)
		o.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no OTLP request (wanted %d)", n)
}

type otlpDoc struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []otlpKV `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				TimeUnixNano   string   `json:"timeUnixNano"`
				SeverityNumber int      `json:"severityNumber"`
				SeverityText   string   `json:"severityText"`
				Body           otlpVal  `json:"body"`
				Attributes     []otlpKV `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type otlpKV struct {
	Key   string  `json:"key"`
	Value otlpVal `json:"value"`
}

type otlpVal struct {
	StringValue string `json:"stringValue"`
	IntValue    string `json:"intValue"`
}

func attrOf(kvs []otlpKV, key string) (otlpVal, bool) {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return otlpVal{}, false
}

func TestOTLPShipsRedactedRowsAsOTLPHTTPJSON(t *testing.T) {
	sink := &otlpSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	o := dblog.NewOTLP(dblog.OTLPOptions{Endpoint: srv.URL + "/v1/logs", Headers: map[string]string{"Authorization": "Bearer collector-token", "X-Tenant": "t1"},
		Service: "paymux", Version: "1.2.3", FlushEvery: 5 * time.Millisecond, Stdout: slog.New(slog.DiscardHandler)})
	o.Start()
	defer o.Close()
	at := time.Date(2026, 10, 19, 12, 0, 0, 123_000_000, time.UTC)
	o.Ship([]store.LogRow{
		{ID: "log_1", At: at, Level: "error", Kind: "provider", Component: "connector:payu", Msg: "provider call failed: POST x -> 504", RequestID: "req_1", PaymentID: "pay_1",
			Connector: "payu", IDs: `{"provider_request_id":"preq_1"}`, Attrs: `{"failure":"status"}`, Request: `{"method":"POST"}`, Response: `{"status":504}`, Status: 504, DurationMS: 1500},
		{ID: "log_2", At: at, Level: "warn", Kind: "http", Component: "http", Msg: "POST /v1/ -> 401", Status: 401},
	})
	sink.wait(t, 1)
	sink.mu.Lock()
	body, hdr, path := sink.bodies[0], sink.headers[0], sink.paths[0]
	sink.mu.Unlock()
	if path != "/v1/logs" || hdr.Get("Content-Type") != "application/json" || hdr.Get("Authorization") != "Bearer collector-token" || hdr.Get("X-Tenant") != "t1" {
		t.Errorf("%s %v", path, hdr)
	}
	var doc otlpDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if len(doc.ResourceLogs) != 1 || len(doc.ResourceLogs[0].ScopeLogs) != 1 {
		t.Fatalf("%s", body)
	}
	if v, _ := attrOf(doc.ResourceLogs[0].Resource.Attributes, "service.name"); v.StringValue != "paymux" {
		t.Errorf("service.name: %s", body)
	}
	if v, _ := attrOf(doc.ResourceLogs[0].Resource.Attributes, "service.version"); v.StringValue != "1.2.3" {
		t.Errorf("service.version")
	}
	recs := doc.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 2 {
		t.Fatalf("%d records: %s", len(recs), body)
	}
	r := recs[0]
	if r.SeverityNumber != 17 || r.SeverityText != "ERROR" || r.Body.StringValue != "provider call failed: POST x -> 504" || r.TimeUnixNano != strconv.FormatInt(at.UnixNano(), 10) {
		t.Errorf("record: %+v", r)
	}
	for k, want := range map[string]string{"log.id": "log_1", "kind": "provider", "component": "connector:payu", "request_id": "req_1", "payment_id": "pay_1",
		"connector": "payu", "provider_request_id": "preq_1", "http.request": `{"method":"POST"}`, "http.response": `{"status":504}`, "attrs": `{"failure":"status"}`} {
		if v, ok := attrOf(r.Attributes, k); !ok || v.StringValue != want {
			t.Errorf("attribute %s = %+v (%v), want %q", k, v, ok, want)
		}
	}
	if v, _ := attrOf(r.Attributes, "http.status_code"); v.IntValue != "504" {
		t.Errorf("status: %+v", v)
	}
	if recs[1].SeverityNumber != 13 || recs[1].SeverityText != "WARN" {
		t.Errorf("warn record: %+v", recs[1])
	}
}

func TestOTLPNeverBlocksOrCrashesWhenTheCollectorIsDown(t *testing.T) {
	sink := &otlpSink{status: 503}
	srv := httptest.NewServer(sink)
	var out strings.Builder
	o := dblog.NewOTLP(dblog.OTLPOptions{Endpoint: srv.URL, QueueSize: 4, FlushEvery: 5 * time.Millisecond,
		Stdout: slog.New(slog.NewTextHandler(&lockedWriter{w: &out}, nil))})
	o.Start()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			o.Ship([]store.LogRow{{ID: "log_x", At: time.Now(), Level: "error", Kind: "app", Component: "t", Msg: "m"}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Ship blocked")
	}
	sink.wait(t, 1) // it did try
	srv.Close()     // and now the collector is gone altogether
	o.Ship([]store.LogRow{{ID: "log_y", At: time.Now(), Level: "error", Kind: "app", Component: "t", Msg: "m"}})
	time.Sleep(50 * time.Millisecond)
	o.Close()
	o.Ship([]store.LogRow{{ID: "log_z"}}) // after Close: ignored, no panic
	if o.Dropped() == 0 {
		t.Error("overflow is not counted")
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// The recorder hands every batch it writes to Tee, so the OTLP export sees
// exactly the rows the database does.
func TestRecorderTeesBatchesToTheOTLPShipper(t *testing.T) {
	sink := &otlpSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	o := dblog.NewOTLP(dblog.OTLPOptions{Endpoint: srv.URL, FlushEvery: 5 * time.Millisecond, Stdout: slog.New(slog.DiscardHandler)})
	o.Start()
	defer o.Close()
	e := newEnv(t, storetest.SQLite(t), func(opt *dblog.Options) { opt.Tee = o.Ship })
	e.log.Error("shipped " + strings.Repeat("x", 3))
	e.log.Info("not shipped")
	e.rec.Flush(context.Background())
	sink.wait(t, 1)
	sink.mu.Lock()
	body := string(sink.bodies[0])
	sink.mu.Unlock()
	if !strings.Contains(body, "shipped xxx") || strings.Contains(body, "not shipped") {
		t.Errorf("%s", body)
	}
}
