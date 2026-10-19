package cashfree

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// signedAt is a webhook signed over the timestamp ts (milliseconds) and received
// at receivedMS.
func signedAt(t *testing.T, ts string, receivedMS int64) connector.WebhookRequest {
	t.Helper()
	body := fixture(t, "event_payment_success")
	return connector.WebhookRequest{Method: "POST", Body: body, ReceivedAt: receivedMS,
		Headers: map[string][]string{"X-Webhook-Signature": {sign(hookSecret, ts, body)}, "X-Webhook-Timestamp": {ts}}}
}

// L2: the signature covers timestamp + body, so the timestamp can bound a replay.
// Cashfree retries for about a day; the window is 48 hours either way.
func TestWebhookTimestampOutsideTheWindowIsRefused(t *testing.T) {
	c := newConn(t, newFake())
	ctx := context.Background()
	ms := func(d time.Duration) string { return strconv.FormatInt(time.Unix(nowUnix, 0).Add(d).UnixMilli(), 10) }
	received := nowUnix * 1000
	for _, tc := range []struct {
		name string
		ts   string
		ok   bool
	}{
		{"now", ms(0), true},
		{"23 hours old (a retry)", ms(-23 * time.Hour), true},
		{"47 hours old", ms(-47 * time.Hour), true},
		{"49 hours old", ms(-49 * time.Hour), false},
		{"a week old", ms(-7 * 24 * time.Hour), false},
		{"from the future", ms(49 * time.Hour), false},
		{"seconds, in range", strconv.FormatInt(nowUnix, 10), true},
		{"seconds, a week old", strconv.FormatInt(nowUnix-7*24*3600, 10), false},
		{"not a number", "yesterday", false},
	} {
		err := c.VerifyWebhook(ctx, signedAt(t, tc.ts, received))
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, connector.ErrInvalidSignature)) {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	// With no receipt time the age cannot be judged (the signature still must hold).
	if err := c.VerifyWebhook(ctx, signedAt(t, ms(-30*24*time.Hour), 0)); err != nil {
		t.Errorf("unknown receipt time: %v", err)
	}
}

// L2: x-idempotency-key is not covered by the signature (only timestamp + body
// are), so it cannot name an event: the id comes from the body, and a replay with
// another key is the same event.
func TestWebhookEventIDIgnoresTheUnsignedIdempotencyKey(t *testing.T) {
	c := newConn(t, newFake())
	ctx := context.Background()
	id := func(key string) string {
		req := hook(fixture(t, "event_payment_success"), hookSecret)
		if key != "" {
			req.Headers["X-Idempotency-Key"] = []string{key}
		}
		ns, err := c.ParseWebhook(ctx, req)
		if err != nil || len(ns) != 1 {
			t.Fatalf("%v %v", ns, err)
		}
		return ns[0].EventID
	}
	a, b, none := id("key-a"), id("key-b"), id("")
	if a != b || a != none || a == "" {
		t.Errorf("a replay with another key must be the same event: %q %q %q", a, b, none)
	}
	if strings.Contains(a, "key-a") {
		t.Errorf("the unsigned header leaked into the id: %q", a)
	}
	// A retry (new timestamp, same body) is the same event too.
	req := hook(fixture(t, "event_payment_success"), hookSecret)
	req.Headers["X-Webhook-Timestamp"] = []string{"1760000999999"}
	if ns, _ := c.ParseWebhook(ctx, req); len(ns) != 1 || ns[0].EventID != a {
		t.Errorf("a retry got another id: %v", ns)
	}
	// A different payload is a different event.
	other := strings.Replace(string(fixture(t, "event_payment_success")), "SUCCESS", "FAILED", 1)
	if ns, _ := c.ParseWebhook(ctx, hook([]byte(other), hookSecret)); len(ns) == 1 && ns[0].EventID == a {
		t.Error("two payloads share an id")
	}
}
