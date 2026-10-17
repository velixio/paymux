package payu

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Fuzz targets run as plain tests over their seed corpus in CI; run one for
// real with: go test -run xxx -fuzz FuzzVerifyWebhook -fuzztime 2m ./connectors/payu

func fuzzConn(tb testing.TB) connector.Connector {
	tb.Helper()
	c, err := New(testConfig, connectortest.NewDeps(nil, time.Unix(1760000100, 0)))
	if err != nil {
		tb.Fatal(err)
	}
	return c
}

func goldenForms() []url.Values {
	failed := successForm(testSalt, "")
	failed.Set("status", "failure")
	failed.Set("hash", reverseHash(testSalt, failed))
	return []url.Values{successForm(testSalt, ""), successForm(testSalt, "12.50"), failed}
}

// authenticated are the fields PayU's reverse hash covers. Every other field
// (mihpayid, mode, addedon, ...) travels unsigned, so a body that differs only
// there still verifies; the engine fetches the payment before trusting it.
var authenticated = []string{"status", "udf1", "udf2", "udf3", "udf4", "udf5", "email", "firstname", "productinfo", "amount", "txnid", "key",
	"additionalCharges", "additional_charges", "hash"}

func sameAuthenticated(a, b url.Values) bool {
	for _, k := range authenticated {
		x, y := a.Get(k), b.Get(k)
		if k == "hash" { // hex digits: the case is not authenticated
			x, y = strings.ToLower(x), strings.ToLower(y)
		}
		if x != y {
			return false
		}
	}
	return true
}

func FuzzVerifyWebhook(f *testing.F) {
	golden := goldenForms()
	for i, g := range golden {
		f.Add(uint8(i), []byte(keyLast(g)))
	}
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, idx uint8, body []byte) {
		orig := golden[int(idx)%len(golden)]
		err := c.VerifyWebhook(context.Background(), webhookBody(body))
		if err != nil && !errors.Is(err, connector.ErrInvalidSignature) && !errors.Is(err, connector.ErrUnsigned) {
			t.Fatalf("untyped error: %v", err)
		}
		if err != nil || bytes.Equal(body, []byte(keyLast(orig))) {
			return
		}
		// Verified but different: only unauthenticated fields may differ from
		// the golden form it was derived from, or from some other golden form.
		got, perr := url.ParseQuery(string(body))
		if perr != nil {
			t.Fatalf("verified an unparsable body: %q", body)
		}
		for _, g := range golden {
			if sameAuthenticated(got, g) {
				return
			}
		}
		t.Fatalf("a body with altered authenticated fields verified under the fixture salt: %q", body)
	})
}

func webhookBody(b []byte) connector.WebhookRequest {
	return connector.WebhookRequest{Method: "POST", Body: b, ReceivedAt: 1,
		Headers: map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}}}
}

func FuzzParseWebhook(f *testing.F) {
	for _, g := range goldenForms() {
		f.Add([]byte(keyLast(g)))
	}
	f.Add([]byte(""))
	c := fuzzConn(f)
	f.Fuzz(func(t *testing.T, body []byte) {
		ns, err := c.ParseWebhook(context.Background(), webhookBody(body))
		if err != nil {
			return
		}
		if err := connectortest.CheckNotifications(ns); err != nil {
			t.Fatalf("%v\nbody: %q", err, body)
		}
	})
}
