package whop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Fuzz targets run as plain tests over their seed corpus in CI; run one for
// real with: go test -run xxx -fuzz FuzzVerifyWebhook -fuzztime 2m ./connectors/whop

func fuzzConn(tb testing.TB) connector.Connector {
	tb.Helper()
	c, err := New(testConfig, connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

// goldenWebhooks are the signed golden fixtures (the webhook-shaped ones).
func goldenWebhooks(tb testing.TB) []connector.WebhookRequest {
	tb.Helper()
	files, err := filepath.Glob("testdata/*.json")
	if err != nil || len(files) == 0 {
		tb.Fatalf("no fixtures: %v", err)
	}
	var out []connector.WebhookRequest
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			tb.Fatal(err)
		}
		if !strings.Contains(string(b), `"type"`) {
			continue // an API response, not an event
		}
		id := "msg_" + strings.TrimSuffix(filepath.Base(f), ".json")
		out = append(out, connector.WebhookRequest{Method: "POST", Body: b, ReceivedAt: nowUnix * 1000, Headers: map[string][]string{
			"Webhook-Id": {id}, "Webhook-Timestamp": {"1760000000"}, "Webhook-Signature": {sign([]byte(testSecret), id, nowUnix, b)}}})
	}
	if len(out) == 0 {
		tb.Fatal("no webhook fixtures")
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

func FuzzParseWebhook(f *testing.F) {
	for _, w := range goldenWebhooks(f) {
		f.Add(w.Body, w.Header("Webhook-Id"))
	}
	f.Add([]byte(`{}`), "")
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, body []byte, id string) {
		ns, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Method: "POST", Body: body,
			Headers: map[string][]string{"Webhook-Id": {id}}})
		if err != nil {
			return
		}
		if err := connectortest.CheckNotifications(ns); err != nil {
			t.Fatalf("%v\nbody: %q", err, body)
		}
	})
}
