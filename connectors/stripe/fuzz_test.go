package stripe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Fuzz targets run as plain tests over their seed corpus in CI; run one for
// real with: go test -run xxx -fuzz FuzzVerifyWebhook -fuzztime 2m ./connectors/stripe

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
		out = append(out, hook(b, testSecret, nowUnix))
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
		if err != nil && !errors.Is(err, connector.ErrInvalidSignature) && !errors.Is(err, connector.ErrUnsigned) {
			t.Fatalf("untyped error: %v", err)
		}
		if err == nil && !bytes.Equal(body, orig.Body) {
			t.Fatalf("a mutated body verified under the fixture secret: %q", body)
		}
	})
}

// The signature header itself is attacker-controlled too: whatever it holds, a
// delivery signed for another body must not verify.
func FuzzSignatureHeader(f *testing.F) {
	golden := goldenWebhooks(f)
	f.Add(golden[0].Header("Stripe-Signature"))
	f.Add("t=1760000100,v1=")
	f.Add("v1=zz,t=")
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, hdr string) {
		req := golden[0]
		req.Headers = map[string][]string{"Stripe-Signature": {hdr}}
		err := c.VerifyWebhook(context.Background(), req)
		if err != nil && !errors.Is(err, connector.ErrInvalidSignature) {
			t.Fatalf("untyped error: %v", err)
		}
		if err == nil && hdr != golden[0].Header("Stripe-Signature") {
			// Only an equivalent header (same t, the right v1 among others) may verify.
			want := sign(testSecret, nowUnix, req.Body)
			if !bytes.Contains(bytes.ToLower([]byte(hdr)), []byte(want)) { // hex digits are case-insensitive
				t.Fatalf("verified without the right signature: %q", hdr)
			}
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

// toStripe and fromStripe agree wherever the first succeeds.
func FuzzAmounts(f *testing.F) {
	for _, c := range []string{"USD", "JPY", "MGA", "ISK", "KWD", "INR", "XXX"} {
		f.Add(int64(49900), c)
		f.Add(int64(0), c)
	}
	f.Fuzz(func(t *testing.T, minor int64, cur string) {
		if minor < 0 {
			return
		}
		v, err := toStripe(minor, cur)
		if err != nil {
			return
		}
		if back := fromStripe(v, cur); back != minor {
			t.Fatalf("%d %s -> %d -> %d", minor, cur, v, back)
		}
	})
}
