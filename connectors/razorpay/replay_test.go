package razorpay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// L2: X-Razorpay-Event-Id is not covered by the signature, so it cannot name an
// event: a captured signed body replayed with another header id would have been
// applied again. The id comes from the signed body alone.
func TestEventIDIsDerivedFromTheSignedBodyNotTheHeader(t *testing.T) {
	c := newConn(t, newFake("created"))
	ctx := context.Background()
	for _, file := range []string{"event_payment_captured", "event_subscription_activated"} {
		a, errA := c.ParseWebhook(ctx, webhookReq(t, file, testWebhookSecret, "evt_a"))
		b, errB := c.ParseWebhook(ctx, webhookReq(t, file, testWebhookSecret, "evt_b"))
		retry, _ := c.ParseWebhook(ctx, webhookReq(t, file, testWebhookSecret, "evt_a"))
		if errA != nil || errB != nil || len(a) != 1 || len(b) != 1 || len(retry) != 1 {
			t.Fatalf("%s: %v %v", file, errA, errB)
		}
		if a[0].EventID != b[0].EventID {
			t.Errorf("%s: a replay with another header id got a new id (%q, %q): it would be applied twice", file, a[0].EventID, b[0].EventID)
		}
		if a[0].EventID != retry[0].EventID || a[0].EventID == "" {
			t.Errorf("%s: a retry of the same delivery must dedupe: %q vs %q", file, a[0].EventID, retry[0].EventID)
		}
		if strings.Contains(a[0].EventID, "evt_a") {
			t.Errorf("%s: the unsigned header leaked into the id: %q", file, a[0].EventID)
		}
	}
	x, _ := c.ParseWebhook(ctx, webhookReq(t, "event_payment_captured", testWebhookSecret, "e"))
	y, _ := c.ParseWebhook(ctx, webhookReq(t, "event_payment_failed", testWebhookSecret, "e"))
	if len(x) == 1 && len(y) == 1 && x[0].EventID == y[0].EventID {
		t.Error("two different events share an id")
	}
}

// L2: a subscription event is applied from its payload, so a captured signed
// delivery must not be replayable for ever. The signed created_at is checked
// against the time of receipt: Razorpay retries for about a day, so the window is
// 48 hours either way. Payment, refund and dispute events are not checked: they
// are applied from a fresh fetch, which repeats nothing.
func TestSubscriptionEventsOutsideTheWindowAreRefused(t *testing.T) {
	c := newConn(t, newFake("created"))
	ctx := context.Background()
	created := int64(1760000100) // created_at of the fixtures
	at := func(d time.Duration) int64 { return time.Unix(created, 0).Add(d).UnixMilli() }
	for _, tc := range []struct {
		name string
		at   int64
		ok   bool
	}{
		{"just after", at(time.Minute), true},
		{"the retries of a day", at(23 * time.Hour), true},
		{"47 hours", at(47 * time.Hour), true},
		{"49 hours", at(49 * time.Hour), false},
		{"a week", at(7 * 24 * time.Hour), false},
		{"far in the future", at(-49 * time.Hour), false},
		{"unknown receipt time", 0, true},
	} {
		r := webhookReq(t, "event_subscription_charged", testWebhookSecret, "e")
		r.ReceivedAt = tc.at
		err := c.VerifyWebhook(ctx, r)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, connector.ErrInvalidSignature)) {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	// A payment event is not time-checked.
	r := webhookReq(t, "event_payment_captured", testWebhookSecret, "e")
	r.ReceivedAt = at(30 * 24 * time.Hour)
	if err := c.VerifyWebhook(ctx, r); err != nil {
		t.Errorf("a late payment event: %v", err)
	}
	// A subscription event without a created_at cannot be placed in time.
	body := `{"entity":"event","event":"subscription.activated","payload":{"subscription":{"entity":{"id":"sub_Nx5"}}}}`
	r = connector.WebhookRequest{Method: "POST", Body: []byte(body), ReceivedAt: at(time.Minute),
		Headers: map[string][]string{"X-Razorpay-Signature": {sign(testWebhookSecret, []byte(body))}}}
	if err := c.VerifyWebhook(ctx, r); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Errorf("no created_at: %v", err)
	}
}
