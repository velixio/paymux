package jsvm_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/pkg/jsvm"
)

var ctx = context.Background()

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type harness struct {
	t      *testing.T
	dir    string
	logs   *logBuf
	errs   []string
	mu     sync.Mutex
	rt     *jsvm.Runtime
	client *http.Client
}

func (h *harness) onError(hook string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, hook+": "+err.Error())
}

func (h *harness) reported() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.errs, "\n")
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup writes the hook files and builds a runtime. The default client allows
// loopback (so httptest servers work); tests of the SSRF guard swap it.
func setup(t *testing.T, files map[string]string, mod func(*jsvm.Options, *harness)) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), logs: &logBuf{}, client: outbound.GuardedClient(true, 5*time.Second)}
	for name, body := range files {
		write(t, h.dir, name, body)
	}
	o := jsvm.Options{Dir: h.dir, Logger: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), OnError: h.onError}
	if mod != nil {
		mod(&o, h)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = h.client
	}
	rt, err := jsvm.New(o)
	if err != nil {
		t.Fatal(err)
	}
	h.rt = rt
	t.Cleanup(rt.Close)
	return h
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

type slack struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []string
	ct   []string
}

func newSlack(t *testing.T) *slack {
	s := &slack{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hits = append(s.hits, r.Method+" "+r.URL.Path+" "+string(b))
		s.ct = append(s.ct, r.Header.Get("Content-Type"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"echo":` + strings.TrimSpace(string(b)) + `}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *slack) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.hits) }

var paymentEvent = map[string]any{
	"event":   map[string]any{"type": "payment.succeeded", "app_id": "app_1", "mode": "live", "object_id": "pay_1"},
	"payment": map[string]any{"id": "pay_1", "amount": 49900, "currency": "INR", "metadata": map[string]any{"plan": "pro"}},
}

func TestHookPostsToASlackWebhook(t *testing.T) {
	sl := newSlack(t)
	h := setup(t, map[string]string{"notify.js": `
		onPaymentSucceeded(function (e) {
			var res = fetch("` + sl.srv.URL + `/services/T/B/x", {
				method: "POST",
				headers: {"Content-Type": "application/json"},
				body: JSON.stringify({text: "Paid " + e.payment.amount / 100 + " " + e.payment.currency + " (" + e.payment.metadata.plan + ")"})
			});
			if (!res.ok) throw new Error("slack said " + res.status);
		});
	`}, func(o *jsvm.Options, _ *harness) { o.AllowedHosts = []string{hostOf(t, sl.srv.URL)} })

	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
		t.Fatalf("hook failed: %v\n%s", err, h.logs)
	}
	if sl.count() != 1 || sl.hits[0] != `POST /services/T/B/x {"text":"Paid 499 INR (pro)"}` || sl.ct[0] != "application/json" {
		t.Errorf("slack saw %v %v", sl.hits, sl.ct)
	}
}

func TestPromisesAndAsyncFunctionsWork(t *testing.T) {
	sl := newSlack(t)
	h := setup(t, map[string]string{"a.js": `
		onRefundSucceeded(async function (e) {
			const res = await fetch("` + sl.srv.URL + `/r", {method: "POST", body: JSON.stringify({id: e.refund.id})});
			const data = await res.json();
			if (data.echo.id !== e.refund.id) throw new Error("bad echo " + JSON.stringify(data));
		});
		onDisputeOpened(function (e) {
			return fetch("` + sl.srv.URL + `/d", {method: "POST", body: "{}"}).then(function (r) { return r.text(); }).then(function (t) {
				if (t.indexOf('"ok":true') < 0) throw new Error("bad body " + t);
			});
		});
	`}, func(o *jsvm.Options, _ *harness) { o.AllowedHosts = []string{hostOf(t, sl.srv.URL)} })
	if err := h.rt.Call(ctx, "onRefundSucceeded", map[string]any{"refund": map[string]any{"id": "ref_1"}}); err != nil {
		t.Fatalf("async hook: %v", err)
	}
	if err := h.rt.Call(ctx, "onDisputeOpened", map[string]any{}); err != nil {
		t.Fatalf("promise hook: %v", err)
	}
	if sl.count() != 2 {
		t.Errorf("hits %v", sl.hits)
	}
	// A rejected promise is a failure.
	write(t, h.dir, "b.js", `onInvoiceIssued(async function () { throw new Error("boom in async"); });`)
	if err := h.rt.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := h.rt.Call(ctx, "onInvoiceIssued", map[string]any{}); err == nil || !strings.Contains(err.Error(), "boom in async") {
		t.Errorf("rejected promise: %v", err)
	}
}

func TestAnInfiniteLoopIsKilledAfterTwoSeconds(t *testing.T) {
	h := setup(t, map[string]string{"loop.js": `
		onPaymentSucceeded(function () { while (true) {} });
		onRoute(function () { for (;;) {} });
	`, "ok.js": `onRefundSucceeded(function (e) { console.log("refund " + e.refund.id); });`}, nil)

	start := time.Now()
	err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("err = %v", err)
	}
	if took < 1900*time.Millisecond || took > 60*time.Second {
		t.Errorf("killed after %v, want about 2s", took)
	}

	// Routing falls back: no suggestion and an error the caller can ignore.
	start = time.Now()
	order, err := h.rt.Route(ctx, map[string]any{"currency": "INR"})
	if order != nil || err == nil || time.Since(start) > 60*time.Second {
		t.Errorf("route: %v %v after %v", order, err, time.Since(start))
	}

	// The runtime recovers: other hooks still run.
	if err := h.rt.Call(ctx, "onRefundSucceeded", map[string]any{"refund": map[string]any{"id": "ref_7"}}); err != nil {
		t.Errorf("after a timeout: %v", err)
	}
	if !strings.Contains(h.logs.String(), "refund ref_7") {
		t.Errorf("console.log did not reach the logger:\n%s", h.logs)
	}
}

func TestTimeSpentInFetchIsNotCountedAgainstTheScriptBudget(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	}))
	defer slow.Close()
	h := setup(t, map[string]string{"s.js": `
		onPaymentSucceeded(function () {
			fetch("` + slow.URL + `");
			fetch("` + slow.URL + `");
		});`}, func(o *jsvm.Options, _ *harness) { o.AllowedHosts = []string{hostOf(t, slow.URL)} })
	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
		t.Errorf("3s of waiting on a provider is not 3s of script: %v", err)
	}
}

func TestFetchToADisallowedHostIsRejected(t *testing.T) {
	sl := newSlack(t)
	h := setup(t, map[string]string{"x.js": `
		onPaymentSucceeded(function () { fetch("` + sl.srv.URL + `/steal", {method: "POST", body: "secret"}); });
		onRefundSucceeded(function () {
			try { fetch("` + sl.srv.URL + `/steal"); return; } catch (e) { throw new Error("caught: " + e.message); }
		});
	`}, func(o *jsvm.Options, _ *harness) { o.AllowedHosts = []string{"hooks.slack.com", "*.example.com"} })
	err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
	if err == nil || !strings.Contains(err.Error(), "not allowed") || !strings.Contains(err.Error(), hostOf(t, sl.srv.URL)) {
		t.Errorf("disallowed host: %v", err)
	}
	if sl.count() != 0 {
		t.Errorf("a request went out to a host that is not allowed: %v", sl.hits)
	}
	// The script can catch it like any error.
	err = h.rt.Call(ctx, "onRefundSucceeded", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "caught:") || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("catchable: %v", err)
	}
}

func TestAllowedHostMatching(t *testing.T) {
	h := setup(t, map[string]string{"x.js": `
		onPaymentSucceeded(function (e) {
			var out = [];
			["https://hooks.slack.com/a", "https://api.example.com/b", "https://example.com/c", "https://evil-example.com/d",
			 "https://hooks.slack.com.evil.test/e", "ftp://hooks.slack.com/f", "file:///etc/passwd", "//hooks.slack.com/g", "https://user:pw@hooks.slack.com@evil.test/h"].forEach(function (u) {
				try { fetch(u); out.push("@@" + u + "|reached"); } catch (err) { out.push("@@" + u + "|" + err.message); }
			});
			throw new Error(out.join("\n"));
		});`}, func(o *jsvm.Options, _ *harness) {
		o.AllowedHosts = []string{"hooks.slack.com", "*.example.com"}
		o.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial blocked in test")
		})}
	})
	err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
	if err == nil {
		t.Fatal("expected the report")
	}
	got := map[string]string{}
	for _, l := range strings.Split(err.Error(), "\n") {
		if i := strings.Index(l, "@@"); i >= 0 {
			if u, msg, ok := strings.Cut(l[i+2:], "|"); ok {
				got[u] = msg
			}
		}
	}
	allowed := func(u string) bool { return strings.Contains(got[u], "dial blocked in test") }
	for _, u := range []string{"https://hooks.slack.com/a", "https://api.example.com/b"} {
		if !allowed(u) {
			t.Errorf("%s should pass the host check: %q", u, got[u])
		}
	}
	for _, u := range []string{"https://example.com/c", "https://evil-example.com/d", "https://hooks.slack.com.evil.test/e", "ftp://hooks.slack.com/f",
		"file:///etc/passwd", "//hooks.slack.com/g", "https://user:pw@hooks.slack.com@evil.test/h"} {
		if allowed(u) || got[u] == "" {
			t.Errorf("%s must be refused: %q", u, got[u])
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPrivateAddressIsRejectedEvenForAnAllowedHost(t *testing.T) {
	sl := newSlack(t) // 127.0.0.1
	h := setup(t, map[string]string{"x.js": `
		onPaymentSucceeded(function () { fetch("` + sl.srv.URL + `/internal"); });`}, func(o *jsvm.Options, _ *harness) {
		o.AllowedHosts = []string{hostOf(t, sl.srv.URL)}
		o.HTTPClient = outbound.GuardedClient(false, 5*time.Second) // the production guard
	})
	err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Errorf("private address: %v", err)
	}
	if sl.count() != 0 {
		t.Error("the request reached a private address")
	}
}

func TestFetchSizeAndMethodLimits(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1<<20+10))
	}))
	defer big.Close()
	small := newSlack(t)
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer okSrv.Close()
	h := setup(t, map[string]string{"x.js": `
		onPaymentSucceeded(function () { fetch("` + big.URL + `").text(); });
		onRefundSucceeded(function () { fetch("` + small.srv.URL + `", {method: "POST", body: "y".repeat(1024 * 1024 + 1)}); });
		onDisputeOpened(function () { fetch("` + small.srv.URL + `", {method: "CONNECT"}); });
		onInvoiceIssued(function () { var r = fetch("` + okSrv.URL + `", {method: "POST", body: "y".repeat(1024 * 1024)}); if (!r.ok) throw new Error("not ok"); });
	`}, func(o *jsvm.Options, _ *harness) { o.AllowedHosts = []string{hostOf(t, big.URL)} })
	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("response over 1 MB: %v", err)
	}
	if err := h.rt.Call(ctx, "onRefundSucceeded", map[string]any{}); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("request over 1 MB: %v", err)
	}
	if err := h.rt.Call(ctx, "onDisputeOpened", map[string]any{}); err == nil || !strings.Contains(err.Error(), "method") {
		t.Errorf("odd method: %v", err)
	}
	if err := h.rt.Call(ctx, "onInvoiceIssued", map[string]any{}); err != nil {
		t.Errorf("a request of exactly 1 MB is allowed: %v", err)
	}
}

// The scripts below use eval only to check that it cannot reach anything the
// sandbox withholds; no Go-side eval is involved.
func TestAHookCannotReachRequireOrTheFilesystem(t *testing.T) {
	h := setup(t, map[string]string{"x.js": `
		onPaymentSucceeded(function () {
			var present = [];
			["require", "module", "exports", "process", "Buffer", "fs", "os", "child_process", "Deno", "Bun", "XMLHttpRequest", "WebSocket",
			 "setTimeout", "setInterval", "importScripts", "readFile", "open", "__dirname", "global"].forEach(function (n) {
				if (typeof this[n] !== "undefined" || (function () { try { return typeof eval(n) !== "undefined"; } catch (e) { return false; } })()) present.push(n);
			}, this);
			if (present.length) throw new Error("reachable: " + present.join(","));
		});
		onRefundSucceeded(function () { require("fs").readFileSync("/etc/passwd"); });
	`, "dynamic.js": `onDisputeOpened(function () { import("fs"); });`}, nil)
	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
		t.Errorf("%v", err)
	}
	err := h.rt.Call(ctx, "onRefundSucceeded", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "require") {
		t.Errorf("require: %v", err)
	}
	// Dynamic import() is not part of the runtime: the file is refused at load.
	if !strings.Contains(h.reported(), "dynamic.js") {
		t.Errorf("a script using import() should be rejected at load: %q", h.reported())
	}
}

func TestEventsAreCopiesAndErrorsComeBack(t *testing.T) {
	h := setup(t, map[string]string{"a.js": `
		onPaymentSucceeded(function (e) { e.payment.amount = 1; e.payment.metadata.plan = "tampered"; });
		onPaymentSucceeded(function (e) { if (e.payment.amount !== 49900 || e.payment.metadata.plan !== "pro") throw new Error("saw " + JSON.stringify(e.payment)); });
		onPaymentSucceeded(function () { throw new TypeError("handler three broke"); });
		onPaymentSucceeded(function () { console.log("handler four still runs"); });
	`}, nil)
	err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
	if err == nil || !strings.Contains(err.Error(), "handler three broke") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "saw ") {
		t.Errorf("handlers share state: %v", err)
	}
	if !strings.Contains(h.logs.String(), "handler four still runs") {
		t.Error("a failing handler stopped the ones after it")
	}
	if paymentEvent["payment"].(map[string]any)["amount"] != 49900 {
		t.Error("the Go value was changed by the script")
	}
	// Unregistered hooks are not an error.
	if err := h.rt.Call(ctx, "onInvoiceIssued", map[string]any{}); err != nil {
		t.Errorf("no handler: %v", err)
	}
	if err := h.rt.Call(ctx, "onNothing", map[string]any{}); err == nil {
		t.Error("an unknown hook name must be refused")
	}
}

func TestRouteReturnsTheFirstSuggestion(t *testing.T) {
	h := setup(t, map[string]string{"r.js": `
		onRoute(function (req) { return null; });
		onRoute(function (req) {
			if (req.currency === "INR" && req.accounts.length === 2) return [req.accounts[1].id, "razorpay", 7, null];
		});
		onRoute(function (req) { return ["never"]; });
	`}, nil)
	req := map[string]any{"currency": "INR", "amount": 100, "accounts": []any{
		map[string]any{"id": "pa_1", "connector": "razorpay"}, map[string]any{"id": "pa_2", "connector": "payu"}}}
	order, err := h.rt.Route(ctx, req)
	if err != nil || strings.Join(order, ",") != "pa_2,razorpay" {
		t.Errorf("order %v err %v", order, err)
	}
	if order, err := h.rt.Route(ctx, map[string]any{"currency": "USD", "accounts": []any{}}); err != nil || len(order) != 1 || order[0] != "never" {
		t.Errorf("fallthrough to the third handler: %v %v", order, err)
	}
}

func TestBrokenFilesAreReportedAndTheRestStillLoad(t *testing.T) {
	h := setup(t, map[string]string{
		"good.js":   `onPaymentSucceeded(function () { console.log("good ran"); });`,
		"syntax.js": `onPaymentSucceeded(function () { `,
		"throws.js": `onRefundSucceeded(function () {}); throw new Error("top level failure");`,
		"notes.txt": `not javascript`,
	}, nil)
	rep := h.reported()
	if !strings.Contains(rep, "syntax.js") || !strings.Contains(rep, "throws.js") || strings.Contains(rep, "good.js") || strings.Contains(rep, "notes.txt") {
		t.Errorf("reported:\n%s", rep)
	}
	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil || !strings.Contains(h.logs.String(), "good ran") {
		t.Errorf("good hook: %v\n%s", err, h.logs)
	}
	if hooks := strings.Join(h.rt.Hooks(), ","); !strings.Contains(hooks, "onPaymentSucceeded") {
		t.Errorf("hooks %q", hooks)
	}
}

func TestChangedFilesAreReloaded(t *testing.T) {
	h := setup(t, map[string]string{"v.js": `onPaymentSucceeded(function () { console.log("version one"); });`}, func(o *jsvm.Options, _ *harness) {
		o.PollEvery = 50 * time.Millisecond
	})
	if h.rt.Changed() {
		t.Error("nothing changed yet")
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.rt.Watch(wctx)

	_ = h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
	write(t, h.dir, "v.js", `onPaymentSucceeded(function () { console.log("version two"); });`)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(filepath.Join(h.dir, "v.js"), future, future)
	write(t, h.dir, "w.js", `onRefundSucceeded(function () { console.log("new file"); });`)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.logs.String(), "reloaded") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
		t.Fatal(err)
	}
	_ = h.rt.Call(ctx, "onRefundSucceeded", map[string]any{})
	logs := h.logs.String()
	if !strings.Contains(logs, "version two") || !strings.Contains(logs, "new file") {
		t.Errorf("reload not picked up:\n%s", logs)
	}

	// A broken edit keeps the last good scripts running and is reported.
	write(t, h.dir, "v.js", `onPaymentSucceeded(function () {`)
	if err := h.rt.Reload(); err != nil {
		t.Logf("reload reported: %v", err)
	}
	h.mu.Lock()
	h.logs.mu.Lock()
	h.logs.b.Reset()
	h.logs.mu.Unlock()
	h.mu.Unlock()
	if err := h.rt.Call(ctx, "onRefundSucceeded", map[string]any{}); err != nil || !strings.Contains(h.logs.String(), "new file") {
		t.Errorf("after a broken edit: %v\n%s", err, h.logs)
	}
	if !strings.Contains(h.reported(), "v.js") {
		t.Errorf("broken edit not reported: %s", h.reported())
	}
}

func TestParallelCallsShareAPool(t *testing.T) {
	sl := newSlack(t)
	h := setup(t, map[string]string{"p.js": `
		var calls = 0;
		onPaymentSucceeded(function (e) { calls++; fetch("` + sl.srv.URL + `", {method: "POST", body: JSON.stringify({n: calls})}); });`},
		func(o *jsvm.Options, _ *harness) { o.AllowedHosts = []string{hostOf(t, sl.srv.URL)}; o.PoolSize = 3 })
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if sl.count() != 24 {
		t.Errorf("%d requests", sl.count())
	}
}

func TestCancelledContextStopsAWaitingCall(t *testing.T) {
	block := make(chan struct{})
	h := setup(t, map[string]string{"x.js": `onPaymentSucceeded(function () { while (true) {} });`}, func(o *jsvm.Options, _ *harness) {
		o.PoolSize = 1
		o.CallTimeout = 300 * time.Millisecond
	})
	go func() { _ = h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); close(block) }()
	time.Sleep(50 * time.Millisecond)
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := h.rt.Call(cctx, "onPaymentSucceeded", paymentEvent); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a call waiting for a free runtime must honour its context: %v", err)
	}
	<-block
}
