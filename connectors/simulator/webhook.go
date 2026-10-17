package simulator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

const (
	sigHeader       = "X-Simulator-Signature"
	timestampHeader = "X-Simulator-Timestamp"
	eventIDHeader   = "X-Simulator-Event-Id"
	// webhookTolerance is how far a webhook's timestamp may be from now.
	webhookTolerance = 5 * time.Minute
)

// event is one webhook the simulated provider sends. Exactly the snapshots
// that apply are set; a subscription charge carries both a subscription and
// its payment.
type event struct {
	Type         string                          `json:"type"`
	Payment      *connector.PaymentSnapshot      `json:"payment,omitempty"`
	Refund       *connector.RefundSnapshot       `json:"refund,omitempty"`
	Dispute      *connector.DisputeSnapshot      `json:"dispute,omitempty"`
	Mandate      *connector.MandateSnapshot      `json:"mandate,omitempty"`
	Subscription *connector.SubscriptionSnapshot `json:"subscription,omitempty"`
	Payout       *connector.PayoutSnapshot       `json:"payout,omitempty"`
}

// wireBody is the JSON body of a webhook.
type wireBody struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"` // unix ms
	event
}

func (c *conn) payEvent(typ string, p payRec) event {
	s := p.snapshot()
	return event{Type: typ, Payment: &s}
}

// sign returns the signature of "<timestamp>.<body>".
func sign(secret string, ts int64, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(strconv.FormatInt(ts, 10)))
	h.Write([]byte{'.'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// emit sends the webhook to Paymux, signed, in the background: the caller is
// a customer's click or a job and must not wait for Paymux to process it. A
// failure is only logged; the poller and the return flow reach the same
// result by fetching.
func (c *conn) emit(e event) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := c.send(ctx, e); err != nil {
			c.log.Warn("simulator: sending a webhook", "type", e.Type, "err", err)
		}
	}()
}

func (c *conn) send(ctx context.Context, e event) error {
	if c.deps.URLs == nil || c.deps.HTTP == nil {
		return fmt.Errorf("no URL or HTTP client")
	}
	secret, err := c.secret(ctx)
	if err != nil {
		return err
	}
	now := c.now()
	body, err := json.Marshal(wireBody{ID: newID("evt_sim_"), CreatedAt: now.UnixMilli(), event: e})
	if err != nil {
		return err
	}
	ts := now.Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deps.URLs.WebhookURL(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(timestampHeader, strconv.FormatInt(ts, 10))
	req.Header.Set(sigHeader, sign(secret, ts, body))
	res, err := c.deps.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("paymux answered %s", res.Status)
	}
	return nil
}

func (c *conn) VerifyWebhook(ctx context.Context, req connector.WebhookRequest) error {
	sig, tsRaw := req.Header(sigHeader), req.Header(timestampHeader)
	if sig == "" || tsRaw == "" {
		return connector.ErrInvalidSignature
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return connector.ErrInvalidSignature
	}
	if d := c.now().Sub(time.Unix(ts, 0)); d > webhookTolerance || d < -webhookTolerance {
		return connector.ErrInvalidSignature
	}
	secret, err := c.secret(ctx)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(sig), []byte(sign(secret, ts, req.Body))) {
		return connector.ErrInvalidSignature
	}
	return nil
}

func (c *conn) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var w wireBody
	if err := json.Unmarshal(req.Body, &w); err != nil {
		return nil, fmt.Errorf("simulator: webhook body: %w", err)
	}
	n := connector.Notification{EventID: w.ID, EventType: w.Type, OccurredAt: w.CreatedAt,
		Payment: w.Payment, Refund: w.Refund, Dispute: w.Dispute, Mandate: w.Mandate, Subscription: w.Subscription, Payout: w.Payout}
	switch {
	case w.Payout != nil:
		n.Kind = connector.NotifyPayout
	case w.Subscription != nil:
		n.Kind = connector.NotifySubscription
	case w.Mandate != nil:
		n.Kind = connector.NotifyMandate
		n.Payment = nil
	case w.Dispute != nil:
		n.Kind = connector.NotifyDispute
	case w.Refund != nil:
		n.Kind = connector.NotifyRefund
	case w.Payment != nil:
		n.Kind = connector.NotifyPayment
	default:
		return nil, fmt.Errorf("simulator: webhook %q carries no object", w.Type)
	}
	if err := n.Validate(); err != nil {
		return nil, fmt.Errorf("simulator: %w", err)
	}
	return []connector.Notification{n}, nil
}
