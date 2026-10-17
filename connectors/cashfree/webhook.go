package cashfree

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/paymux/paymux/pkg/connector"
)

// VerifyWebhook checks x-webhook-signature: the base64 HMAC-SHA256 of the
// x-webhook-timestamp header followed by the raw body, under the webhook
// secret. A missing or wrong signature is ErrInvalidSignature.
//
// Decision: the timestamp is covered by the MAC, but no tolerance is applied.
// Cashfree's documentation does not say how old a retried delivery may be, and
// refusing one would lose its event. A replayed delivery only repeats an event
// that the payment fetch (the source of truth) has already confirmed.
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
	return nil
}

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
// events carry no id, so one is derived from the body: a retried delivery is
// byte-identical and dedupes, a later event about the same payment differs.
func (c *cashfree) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var ev event
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("cashfree: bad webhook body: %w", err)
	}
	sum := sha256.Sum256(req.Body)
	eventID := "cf-" + hex.EncodeToString(sum[:16])
	if k := req.Header("x-idempotency-key"); k != "" && len(k) <= 128 {
		eventID = "cf-" + k // webhook versions from 2025-01-01 send one, unique per payload
	}
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
