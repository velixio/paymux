//go:build loadtest

package paymux

// Performance check against the spec section 12 targets. It drives the
// in-process app (SQLite and Postgres) over HTTP and fails when a target is
// missed:
//
//	POST /v1/payments   p99 < 100ms at 20 req/s sustained
//	pay page render     < 1.5s (fake connector, so provider time excluded)
//	invoice PDF render  < 500ms
//	RSS                 < 150MB after 10k payments
//
// Run: go test -tags loadtest -run TestLoad -v -timeout 30m .
// Knobs: LOADTEST_SECONDS (sustained phase, default 20), LOADTEST_CLIENTS
// (concurrent clients, default 8), LOADTEST_PAYMENTS (bulk, default 10000).
// Profile with the usual -cpuprofile / -memprofile flags.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/invoice/render"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/migrations"
	"github.com/paymux/paymux/pkg/connector"
)

const (
	targetCreateP99  = 100 * time.Millisecond
	targetPayPage    = 1500 * time.Millisecond
	targetPDF        = 500 * time.Millisecond
	targetRSSMB      = 150
	sustainedRate    = 20 // requests per second, total
	keyRatePerSecond = 20 // the API's per-key limit; every client gets its own key
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// loadSeed prepares the database: one app with a key per client, and a fake
// provider account whose checkout is an SDK page (so the pay page renders).
func loadSeed(t *testing.T, url, dataDir string, keys int) []string {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, url, dataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	sc := core.OrgScope{OrgID: store.DefaultOrgID}
	svc := core.NewService(core.Options{DB: db, Settings: set})
	app, err := svc.CreateApp(ctx, audit.System, sc, core.CreateAppInput{Name: "Load", Slug: "load"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := 0; i < keys; i++ {
		key, _, err := svc.CreateAPIKey(ctx, audit.System, sc, app.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, key)
	}
	fake := fakeconn.Register("load")
	fake.Locked(func() {
		fake.CheckoutFn = func(r connector.CheckoutRequest) (connector.CheckoutResult, error) {
			return connector.CheckoutResult{ProviderOrderID: "order_" + r.PaymentID, NextAction: connector.NextAction{
				Kind: connector.ActionSDK, ScriptURL: "https://fake.example.com/checkout.js", Options: map[string]any{"key": "k", "order_id": "o"}}}, nil
		}
	})
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := neturl.Parse("http://localhost:8080")
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base})
	if _, err := conns.CreateAccount(ctx, audit.System, sc, connectors.CreateAccountInput{
		Connector: fake.Name, Name: "main", Mode: "test", Config: json.RawMessage(fakeconn.ValidConfig), AppID: app.ID}); err != nil {
		t.Fatal(err)
	}
	doc := `{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + fake.Name + `"}]}}}`
	if err := set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		t.Fatal(err)
	}
	return out
}

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s))*p+0.9999999) - 1
	return s[max(0, min(i, len(s)-1))]
}

func rssMB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
			kb, _ := strconv.ParseFloat(f[1], 64)
			return kb / 1024
		}
	}
	return 0
}

type created struct {
	id, checkoutURL string
	took            time.Duration
}

func TestLoad(t *testing.T) {
	run := func(t *testing.T, url, dataDir string) {
		clients := envInt("LOADTEST_CLIENTS", 8)
		seconds := envInt("LOADTEST_SECONDS", 20)
		bulk := envInt("LOADTEST_PAYMENTS", 10000)
		// Bulk creation uses many keys, each paced under the per-key rate limit,
		// so the limit does not turn it into a 10 minute test.
		bulkClients := max(clients, 32)
		keys := loadSeed(t, url, dataDir, bulkClients)

		cfg := testConfig(t, url, dataDir)
		logs := &syncBuf{}
		app, err := New(cfg, WithLogger(logging.New(logs, slog.LevelWarn)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- app.Start(ctx) }()
		r := &running{app: app, logs: logs, done: make(chan struct{}), stop: cancel}
		go func() { r.err = <-done; close(r.done) }()
		waitReady(t, r)
		defer func() {
			if t.Failed() {
				t.Logf("app logs:\n%s", logs.String())
			}
			shutdown(t, r)
		}()
		base := "http://" + app.Addr()
		hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

		create := func(key string, n int) (created, error) {
			body := fmt.Sprintf(`{"amount":%d,"currency":"INR","order_ref":"load-%d","description":"Load test",`+
				`"customer":{"external_ref":"u%d","name":"Asha","email":"asha@example.com","billing":{"state_code":"29","country":"IN"}}}`, 10000+n%5000, n, n)
			req, _ := http.NewRequest("POST", base+"/v1/payments", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			res, err := hc.Do(req)
			if err != nil {
				return created{}, err
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			took := time.Since(start)
			if res.StatusCode != 201 {
				return created{took: took}, fmt.Errorf("create payment: %d %s", res.StatusCode, b)
			}
			var m struct {
				ID          string `json:"id"`
				CheckoutURL string `json:"checkout_url"`
			}
			_ = json.Unmarshal(b, &m)
			return created{id: m.ID, checkoutURL: m.CheckoutURL, took: took}, nil
		}

		// Phase 1: sustained open-loop load. Requests start on a fixed 20/s
		// schedule regardless of how long earlier ones take, spread over the
		// clients (each with its own key).
		var (
			mu        sync.Mutex
			latencies []time.Duration
			sample    []created
			failures  []error
			wg        sync.WaitGroup
		)
		interval := time.Second / sustainedRate
		total := sustainedRate * seconds
		startAt := time.Now()
		for i := 0; i < total; i++ {
			time.Sleep(time.Until(startAt.Add(time.Duration(i) * interval)))
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, err := create(keys[i%clients], i)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failures = append(failures, err)
					return
				}
				latencies = append(latencies, c.took)
				if c.took > targetCreateP99 {
					t.Logf("slow request #%d (%.1fs in): %v", i, time.Since(startAt).Seconds(), c.took)
				}
				if len(sample) < 60 {
					sample = append(sample, c)
				}
			}(i)
		}
		wg.Wait()
		if len(failures) > 0 {
			t.Fatalf("%d of %d requests failed, first: %v", len(failures), total, failures[0])
		}
		p50, p99, pmax := percentile(latencies, .5), percentile(latencies, .99), percentile(latencies, 1)
		t.Logf("POST /v1/payments at %d req/s for %ds (%d clients): p50=%v p99=%v max=%v (target p99 < %v)",
			sustainedRate, seconds, clients, p50, p99, pmax, targetCreateP99)
		if p99 >= targetCreateP99 {
			t.Errorf("create payment p99 %v >= %v", p99, targetCreateP99)
		}

		// Phase 2: pay page render (fake connector, provider time excluded).
		var pageTimes []time.Duration
		for _, c := range sample {
			start := time.Now()
			res, err := hc.Get(c.checkoutURL2(base))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			took := time.Since(start)
			if res.StatusCode != 200 || !bytes.Contains(b, []byte("fake.example.com/checkout.js")) {
				t.Fatalf("pay page: %d %.200s", res.StatusCode, b)
			}
			pageTimes = append(pageTimes, took)
		}
		pp99 := percentile(pageTimes, .99)
		t.Logf("GET /pay/{id} x%d: p50=%v p99=%v max=%v (target < %v)", len(pageTimes), percentile(pageTimes, .5), pp99, percentile(pageTimes, 1), targetPayPage)
		if pp99 >= targetPayPage {
			t.Errorf("pay page p99 %v >= %v", pp99, targetPayPage)
		}

		// Phase 3: invoice PDF render.
		doc := loadInvoice()
		var pdfTimes []time.Duration
		for i := 0; i < 50; i++ {
			start := time.Now()
			pdf, err := render.Render(doc, render.Options{})
			if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF")) {
				t.Fatalf("render: %v", err)
			}
			pdfTimes = append(pdfTimes, time.Since(start))
		}
		dp99 := percentile(pdfTimes, .99)
		t.Logf("invoice PDF render x%d: p50=%v p99=%v max=%v (target < %v)", len(pdfTimes), percentile(pdfTimes, .5), dp99, percentile(pdfTimes, 1), targetPDF)
		if dp99 >= targetPDF {
			t.Errorf("invoice PDF p99 %v >= %v", dp99, targetPDF)
		}

		// Phase 4: bulk creation (each client as fast as its key's rate limit allows), then
		// the memory check.
		already := len(latencies)
		var next int
		var nextMu sync.Mutex
		var bulkLat []time.Duration
		var bulkErr error
		bulkStart := time.Now()
		for w := 0; w < bulkClients; w++ {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				// Stay under the per-key rate limit (20/s).
				tick := time.NewTicker(time.Second / (keyRatePerSecond - 2))
				defer tick.Stop()
				for {
					<-tick.C
					nextMu.Lock()
					n := next
					next++
					nextMu.Unlock()
					if already+n >= bulk {
						return
					}
					c, err := create(key, already+n)
					nextMu.Lock()
					if err != nil && bulkErr == nil {
						bulkErr = err
					}
					bulkLat = append(bulkLat, c.took)
					nextMu.Unlock()
					if err != nil {
						return
					}
				}
			}(keys[w])
		}
		wg.Wait()
		if bulkErr != nil {
			t.Fatal(bulkErr)
		}
		elapsed := time.Since(bulkStart)
		t.Logf("bulk: %d payments in %v (%.0f/s, %d clients): p50=%v p99=%v", len(bulkLat)+already, elapsed.Round(time.Millisecond),
			float64(len(bulkLat))/elapsed.Seconds(), bulkClients, percentile(bulkLat, .5), percentile(bulkLat, .99))
		hc.CloseIdleConnections()
		runtime.GC()
		debug.FreeOSMemory()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		rss := rssMB()
		t.Logf("memory after %d payments: RSS=%.1fMB heap_inuse=%.1fMB heap_sys=%.1fMB (target RSS < %dMB)", len(bulkLat)+already, rss,
			float64(ms.HeapInuse)/(1<<20), float64(ms.HeapSys)/(1<<20), targetRSSMB)
		if rss >= targetRSSMB {
			t.Errorf("RSS %.1fMB >= %dMB", rss, targetRSSMB)
		}
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "sqlite://"+dir+"/paymux.db", dir)
	})
	t.Run("postgres", func(t *testing.T) { run(t, storetest.PostgresURL(t), t.TempDir()) })
}

// checkoutURL2 rebases the API's checkout_url (built from PAYMUX_BASE_URL) on
// the listener the test is talking to.
func (c created) checkoutURL2(base string) string {
	u, _ := neturl.Parse(c.checkoutURL)
	return base + u.RequestURI()
}

// loadInvoice is a realistic GST invoice: five lines, a logo, inter-state tax.
func loadInvoice() render.Document {
	issued := time.Date(2026, 11, 3, 10, 30, 0, 0, tax.IST)
	d := render.Document{Kind: render.KindInvoice, Number: "VS/2627/000042", IssueDate: issued,
		Supplier: render.Party{LegalName: "Yupcha Technologies Private Limited", TradeName: "Voicestudio",
			Address: "12 MG Road, Indiranagar, Bengaluru 560038", GSTIN: "29ABCDE1234F1Z5", StateName: "Karnataka", StateCode: "29"},
		Recipient:     render.Party{Name: "Priya Sharma", Address: "5 Park Street, Kolkata", StateName: "West Bengal", StateCode: "19", Country: "IN"},
		PlaceOfSupply: "West Bengal (19)", SupplyType: tax.B2C, TemplateVersion: render.Version1,
		PaymentMethod: "UPI", PaymentRef: "pay_9X2KQ7", SignatoryName: "A. Rao", FooterNote: "Thank you for your business."}
	var taxable, igst int64
	for i := 0; i < 5; i++ {
		l := render.Line{Description: fmt.Sprintf("Voicestudio Pro seat %d, Nov 2026", i+1), SAC: "998314", Qty: 1, Unit: "service",
			Taxable: 100000, IGSTRateBP: 1800, IGST: 18000}
		d.Lines = append(d.Lines, l)
		taxable += l.Taxable
		igst += l.IGST
	}
	d.Totals = tax.Amounts{Taxable: taxable, IGST: igst, Total: taxable + igst}
	return d
}
