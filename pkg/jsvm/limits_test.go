package jsvm_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/jsvm"
)

func heapMB() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc >> 20
}

// A hook that tries to take a gigabyte of memory, in one step or by growing,
// is stopped and its VM thrown away; the process and the other hooks carry on.
func TestAHookThatAllocatesAGigabyteIsStopped(t *testing.T) {
	scripts := map[string]string{
		"repeat":          `'x'.repeat(1 << 30).length`,
		"padEnd":          `'x'.padEnd(1 << 30).length`,
		"padStart":        `'x'.padStart(1 << 30, 'ab').length`,
		"fill":            `new Array(1e9).fill(0).length`,
		"Array.from":      `Array.from({length: 1e9}).length`,
		"join":            `new Array(1 << 28).join('xxxxxxxx').length`,
		"concat":          `var a = new Array(1 << 26); a.concat(a, a, a, a, a, a, a, a).length`,
		"apply":           `Array.apply(null, {length: 1e9}).length`,
		"doubling string": `var s = 'x'; for (var i = 0; i < 40; i++) { s += s; s.length; }`,
		"growing array":   `var a = []; while (true) { a.push('x'.repeat(1 << 23)); }`,
		"growing object":  `var o = {}; for (var i = 0; ; i++) { o['k' + i] = 'x'.repeat(1 << 22); }`,
		"growing closure": `var f = []; while (true) { f.push(new Array(1 << 20).fill(1)); }`,
	}
	for name, body := range scripts {
		t.Run(name, func(t *testing.T) {
			hh := setup(t, map[string]string{"big.js": `onPaymentSucceeded(function () { ` + body + ` });
				onRefundSucceeded(function (e) { console.log("alive " + e.refund.id); });`}, func(o *jsvm.Options, _ *harness) {
				o.MaxMemory = 32 << 20
				o.CallTimeout = 10 * time.Second // the memory limit, not the clock, must be what stops it (the race detector is slow)
			})
			start := time.Now()
			err := hh.rt.Call(ctx, "onPaymentSucceeded", paymentEvent)
			if err == nil || !(strings.Contains(err.Error(), "too large") || strings.Contains(err.Error(), "memory")) {
				t.Fatalf("err = %v", err)
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("stopped after %v", took)
			}
			if mb := heapMB(); mb > 400 {
				t.Errorf("%d MB of heap still in use afterwards", mb)
			}
			// Fail closed, and the runtime recovers.
			if err := hh.rt.Call(ctx, "onRefundSucceeded", map[string]any{"refund": map[string]any{"id": "ref_7"}}); err != nil {
				t.Errorf("after the memory limit: %v", err)
			}
			if !strings.Contains(hh.logs.String(), "alive ref_7") {
				t.Errorf("the runtime did not recover:\n%s", hh.logs)
			}
		})
	}
}

// The limit is not in the way of ordinary scripts.
func TestOrdinaryScriptsStillWorkUnderTheMemoryLimit(t *testing.T) {
	h := setup(t, map[string]string{"ok.js": `
		onPaymentSucceeded(function (e) {
			var rows = []; for (var i = 0; i < 20000; i++) rows.push({id: 'pay_' + i, note: 'x'.repeat(100)});
			var csv = rows.map(function (r) { return r.id + ',' + r.note; }).join('\n');
			var padded = '7'.padStart(10, '0') + 'ab'.repeat(1000) + new Array(1000).fill('z').join('');
			console.log("ok " + csv.length + " " + padded.length + " " + [1, 2].concat([3]).length);
		});`}, nil)
	for range 5 {
		if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(h.logs.String(), "ok ") {
		t.Errorf("no output:\n%s", h.logs)
	}
}

// What a hook writes to the log, and what it answers to routing, is bounded.
func TestAHooksOutputIsBounded(t *testing.T) {
	h := setup(t, map[string]string{"o.js": `
		onPaymentSucceeded(function () {
			console.log('x'.repeat(5 << 20));
			for (var i = 0; i < 5000; i++) console.log("line " + i);
		});
		onRoute(function () { var r = []; for (var i = 0; i < 100000; i++) r.push('pa_' + i); return r; });
	`}, nil)
	if err := h.rt.Call(ctx, "onPaymentSucceeded", paymentEvent); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	if len(logs) > 256<<10 {
		t.Errorf("a hook wrote %d bytes to the log in one call", len(logs))
	}
	if n := strings.Count(logs, "js hook: line "); n > 200 {
		t.Errorf("%d console lines from one call", n)
	}
	if !strings.Contains(logs, "truncated") && !strings.Contains(logs, "suppressed") {
		t.Errorf("the log does not say that output was cut:\n%.500s", logs)
	}
	order, err := h.rt.Route(ctx, map[string]any{"currency": "INR"})
	if len(order) > 64 {
		t.Errorf("a routing answer of %d accounts was accepted", len(order))
	}
	_ = err
}

func TestRoutingAnswersAreCappedPerEntry(t *testing.T) {
	h := setup(t, map[string]string{"r.js": fmt.Sprintf(`onRoute(function () { return ["pa_1", %q, "pa_2"]; });`, strings.Repeat("a", 5000))}, nil)
	order, err := h.rt.Route(ctx, map[string]any{})
	if err != nil || strings.Join(order, ",") != "pa_1,pa_2" {
		t.Errorf("order %v err %v", order, err)
	}
}
