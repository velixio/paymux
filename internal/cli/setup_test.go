package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/hub"
)

func TestServeRunsSetupFunctionsOnTheAppBeforeStarting(t *testing.T) {
	dir := t.TempDir()
	base := migrateEnv(dir)
	lookup := func(k string) (string, bool) {
		switch k {
		case "PAYMUX_LISTEN", "PAYMUX_INTERNAL_LISTEN":
			return "127.0.0.1:0", true
		}
		return base(k)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var order []string
	var out, errb bytes.Buffer
	code := RunWith(ctx, []string{"serve"}, lookup, &out, &errb, []func(*hub.App){
		func(a *hub.App) {
			if a == nil {
				t.Error("nil app")
			}
			order = append(order, "first")
			a.OnPaymentSucceeded(nil) // the hooks API is reachable from setup
		},
		func(a *hub.App) {
			order = append(order, "second")
			go func() { // stop once the app is listening
				for a.Addr() == "" {
					time.Sleep(20 * time.Millisecond)
				}
				cancel()
			}()
		},
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("setup order %v", order)
	}
}

func TestRunIsRunWithNoSetup(t *testing.T) {
	code, out, _ := run("version")
	if code != 0 || out != "paymux dev\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
