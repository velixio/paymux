package cashfree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Fuzz targets run as plain tests over their seed corpus in CI; run one for
// real with: go test -run xxx -fuzz FuzzVerifyWebhook -fuzztime 2m ./connectors/cashfree

func fuzzConn(tb testing.TB) connector.Connector {
	tb.Helper()
	c, err := New(testConfig, connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

func goldenWebhooks(tb testing.TB) []connector.WebhookRequest {
	tb.Helper()
	files, err := filepath.Glob("testdata/event_*.json")
	if err != nil || len(files) == 0 {
		tb.Fatalf("no fixtures: %v", err)
	}
	var out []connector.WebhookRequest
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			tb.Fatal(err)
		}
		out = append(out, hook(b, hookSecret))
	}
	return out
}

func FuzzVerifyWebhook(f *testing.F) {
	golden := goldenWebhooks(f)
	for i, w := range golden {
		f.Add(uint8(i), w.Body)
	}
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, idx uint8, body []byte) {
		orig := golden[int(idx)%len(golden)]
		req := orig
		req.Body = body
		err := c.VerifyWebhook(context.Background(), req)
		if err != nil && !errors.Is(err, connector.ErrInvalidSignature) {
			t.Fatalf("untyped error: %v", err)
		}
		if err == nil && !bytes.Equal(body, orig.Body) {
			t.Fatalf("a mutated body verified under the fixture secret: %q", body)
		}
	})
}

// The headers are attacker-controlled too.
func FuzzSignatureHeaders(f *testing.F) {
	golden := goldenWebhooks(f)[0]
	f.Add(golden.Header("x-webhook-signature"), golden.Header("x-webhook-timestamp"))
	f.Add("", "")
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, sig, ts string) {
		req := golden
		req.Headers = map[string][]string{"X-Webhook-Signature": {sig}, "X-Webhook-Timestamp": {ts}}
		err := c.VerifyWebhook(context.Background(), req)
		if err != nil && !errors.Is(err, connector.ErrInvalidSignature) {
			t.Fatalf("untyped error: %v", err)
		}
		if err == nil && ts != golden.Header("x-webhook-timestamp") {
			t.Fatalf("verified under another timestamp: %q", ts)
		}
	})
}

func FuzzParseWebhook(f *testing.F) {
	for _, w := range goldenWebhooks(f) {
		f.Add(w.Body)
	}
	f.Add([]byte(`{}`))
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, body []byte) {
		ns, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Method: "POST", Body: body})
		if err != nil {
			return
		}
		if err := connectortest.CheckNotifications(ns); err != nil {
			t.Fatalf("%v\nbody: %q", err, body)
		}
	})
}

func FuzzAmount(f *testing.F) {
	for _, s := range []string{`499.00`, `"12.5"`, `1e2`, `null`, `-1`, `0.001`, `""`, `"abc"`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var a amount
		if json.Unmarshal(b, &a) != nil {
			return
		}
		minor, err := a.minor("INR")
		if err != nil {
			return
		}
		d, err := decimal(minor, "INR")
		if err != nil {
			return
		}
		if back, err := amount(d).minor("INR"); err != nil || back != minor {
			t.Fatalf("%q -> %d -> %s -> %d (%v)", b, minor, d, back, err)
		}
	})
}

func TestAmountForms(t *testing.T) {
	for in, want := range map[string]int64{`499.00`: 49900, `"12.5"`: 1250, `1e2`: 10000, `null`: 0, `0.1`: 10, `100.50`: 10050} {
		var a amount
		if err := json.Unmarshal([]byte(in), &a); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got, err := a.minor("INR"); err != nil || got != want {
			t.Errorf("%s: %d %v want %d", in, got, err, want)
		}
	}
}
