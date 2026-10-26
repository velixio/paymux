package jsvm_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/pkg/jsvm"
)

// A catastrophic-backtracking regex on event data: does CallTimeout still bound the call?
func TestAudit2RegexBacktrackingIsInterruptible(t *testing.T) {
	dir := t.TempDir()
	src := `onPaymentSucceeded(function(e){ throw new Error("r=" + /^((a+)+)\2$/.test(e.s + "")); });`
	if err := os.WriteFile(filepath.Join(dir, "h.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := jsvm.New(jsvm.Options{Dir: dir, HTTPClient: outbound.GuardedClient(false, time.Second), CallTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- rt.Call(ctx, "onPaymentSucceeded", map[string]string{"s": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab"})
	}()
	select {
	case err := <-done:
		t.Logf("returned after %v: %v", time.Since(start), err)
		if time.Since(start) > 3*time.Second {
			t.Fatalf("call overran its 300ms budget: %v", time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("call still running after 15s with a 300ms CallTimeout (regex not interruptible)")
	}
}
