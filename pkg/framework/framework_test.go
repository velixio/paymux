package framework_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/framework"
	"github.com/paymux/paymux/pkg/hub"
)

func TestBuiltInConnectorsAreLinked(t *testing.T) {
	for _, name := range []string{"razorpay", "payu", "whop", "zoho"} {
		if _, _, ok := connector.Lookup(name); !ok {
			t.Errorf("connector %q is not registered by importing the framework package", name)
		}
	}
}

func TestRunAppliesRegisteredAndPassedSetupToServe(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"PAYMUX_ENV": "development", "PAYMUX_BASE_URL": "http://localhost:8080", "PAYMUX_DATA_DIR": dir,
		"PAYMUX_ENV_FILE": filepath.Join(dir, "none.env"), "PAYMUX_MASTER_KEY": "YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWE=",
		"PAYMUX_DEV_SKIP_2FA": "true", "PAYMUX_LISTEN": "127.0.0.1:0", "PAYMUX_INTERNAL_LISTEN": "127.0.0.1:0",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	var order []string
	framework.OnSetup(func(*hub.App) { order = append(order, "registered") }) // what a plugin module does in init()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	code := framework.Run(ctx, []string{"serve"}, lookup, &out, &errb, func(a *hub.App) {
		order = append(order, "passed")
		go func() {
			for a.Addr() == "" {
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
		}()
	})
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if len(order) != 2 || order[0] != "registered" || order[1] != "passed" {
		t.Errorf("setup order %v", order)
	}
	// Other commands work in a custom binary too.
	out.Reset()
	if code := framework.Run(context.Background(), []string{"version"}, lookup, &out, &errb); code != 0 || out.String() != "paymux dev\n" {
		t.Errorf("version: code=%d out=%q", code, out.String())
	}
}
