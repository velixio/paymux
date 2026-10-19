package cashfree

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// VerifyWebhook checks x-webhook-signature: the base64 HMAC-SHA256 of the
// x-webhook-timestamp header followed by the raw body, under the webhook
// secret. A missing or wrong signature is ErrInvalidSignature.
//
// The timestamp is covered by the MAC, so it bounds a replay: it must be a
// number (epoch milliseconds, or seconds) within webhookWindow of the time of
// receipt, either way, or the delivery is refused as a stale signature.
// Cashfree's documentation does not say how long it retries; the window is 48
// hours, longer than any retry schedule we know of. x-idempotency-key is not
// covered by the MAC and is not used (see ParseWebhook).
func (c *cashfree) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	sig, ts := req.Header("x-webhook-signature"), req.Header("x-webhook-timestamp")
	if sig == "" || ts == "" {
		return connector.ErrInvalidSignature
	}
	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return connector.ErrInvalidSignature
	}
	m := hmac.New(sha256.New, []byte(c.cfg.WebhookSecret))
	m.Write([]byte(ts))
	m.Write(req.Body)
	if !hmac.Equal(m.Sum(nil), got) {
		return connector.ErrInvalidSignature
	}
	if req.ReceivedAt != 0 {
		n, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || n <= 0 {
			return connector.ErrInvalidSignature
		}
		if n < 100_000_000_000 { // seconds, not milliseconds
			n *= 1000
		}
		age := time.Duration(req.ReceivedAt-n) * time.Millisecond
		if age > webhookWindow || age < -webhookWindow {
			return connector.ErrInvalidSignature
		}
	}
	return nil
}

// webhookWindow is how far the signed timestamp may be from the time of receipt.
const webhookWindow = 48 * time.Hour

type event struct {
	Type      string `json:"type"`
	EventTime string `json:"event_time"`
	Data      struct {
		Order struct {
			OrderID  string `json:"order_id"`
			Amount   amount `json:"order_amount"`
			Currency string `json:"order_currency"`
		} `json:"order"`
		Payment *paymentWire `json:"payment"`
		Refund  *refundWire  `json:"refund"`
		// In webhooks error_details is a sibling of payment, not inside it
		// (verified against Cashfree's published payloads).
		ErrorDetails *errorDetails `json:"error_details"`
	} `json:"data"`
}

// ParseWebhook turns an event into Notifications. It makes no requests. Cashfree
// events carry no id the signature covers, so one is derived from
// the body: a retried delivery is byte-identical and dedupes, a later event about the
// same payment differs.
func (c *cashfree) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var ev event
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("cashfree: bad webhook body: %w", err)
	}
	// The id comes from the body, which the signature covers. The x-idempotency-key
	// header (sent by webhook versions from 2025-01-01) is not signed: a captured
	// delivery replayed with another key would have been applied again.
	sum := sha256.Sum256(req.Body)
	eventID := "cf-" + hex.EncodeToString(sum[:16])
	n := connector.Notification{EventID: eventID, EventType: ev.Type, OccurredAt: parseTime(ev.EventTime)}
	switch ev.Type {
	case "PAYMENT_SUCCESS_WEBHOOK", "PAYMENT_FAILED_WEBHOOK", "PAYMENT_USER_DROPPED_WEBHOOK":
		p := ev.Data.Payment
		if p == nil || ev.Data.Order.OrderID == "" {
			return nil, fmt.Errorf("cashfree: %s without an order or a payment", ev.Type)
		}
		if p.Currency == "" {
			p.Currency = ev.Data.Order.Currency
		}
		if p.ErrorDetails == nil {
			p.ErrorDetails = ev.Data.ErrorDetails
		}
		snap := p.snapshot(ev.Data.Order.OrderID)
		n.Kind, n.Payment = connector.NotifyPayment, &snap
	case "REFUND_STATUS_WEBHOOK":
		r := ev.Data.Refund
		if r == nil || r.RefundID == "" {
			return nil, fmt.Errorf("cashfree: %s without a refund", ev.Type)
		}
		snap := r.snapshot()
		n.Kind, n.Refund = connector.NotifyRefund, &snap
	default:
		return nil, nil
	}
	if err := n.Validate(); err != nil {
		return nil, fmt.Errorf("cashfree: %w", err)
	}
	return []connector.Notification{n}, nil
}
