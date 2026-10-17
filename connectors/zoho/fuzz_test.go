package zoho

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Fuzz targets run as plain tests over their seed corpus in CI; run one for
// real with: go test -run xxx -fuzz FuzzVerifyWebhook -fuzztime 2m ./connectors/zoho

var goldenBodies = []string{
	paymentSucceededBody,
	refundSucceededBody,
	`{"event_id":"evt_m1","event_type":"mandate.revoked","payload":{"mandate":{"mandate_id":"md_z1","status":"revoked","max_amount":"748.50","currency":"INR"}}}`,
	`{"event_id":"e","event_type":"mandate.created","payload":{}}`,
}

func fuzzConn(tb testing.TB) connector.Connector {
	tb.Helper()
	c, err := New(widgetCfg, connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

func FuzzVerifyWebhook(f *testing.F) {
	golden := make([]connector.WebhookRequest, len(goldenBodies))
	for i, b := range goldenBodies {
		golden[i] = signedWebhook(testSecret, b, nowUnix, nowUnix)
		f.Add(uint8(i), []byte(b))
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
	for _, b := range goldenBodies {
		f.Add([]byte(b))
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
