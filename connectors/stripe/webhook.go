package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// webhookTolerance is how far a signed timestamp may be from now, either way.
const webhookTolerance = 5 * time.Minute

// VerifyWebhook checks the Stripe-Signature header: "t=<unix>,v1=<hex>[,v1=...]"
// where each v1 is the hex HMAC-SHA256 of "<t>.<raw body>" under the webhook
// secret (several v1 appear while a secret is being rolled). A missing,
// malformed, wrong or stale (outside five minutes) signature is
// ErrInvalidSignature.
func (s *stripe) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	var ts int64
	var sigs []string
	for _, part := range strings.Split(req.Header("Stripe-Signature"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts <= 0 || len(sigs) == 0 {
		return connector.ErrInvalidSignature
	}
	at := s.deps.Now()
	if req.ReceivedAt > 0 {
		at = time.UnixMilli(req.ReceivedAt)
	}
	if d := at.Sub(time.Unix(ts, 0)); d > webhookTolerance || d < -webhookTolerance {
		return connector.ErrInvalidSignature
	}
	m := hmac.New(sha256.New, []byte(s.cfg.WebhookSecret))
	fmt.Fprintf(m, "%d.", ts)
	m.Write(req.Body)
	want := m.Sum(nil)
	for _, sig := range sigs {
		if got, err := hex.DecodeString(sig); err == nil && hmac.Equal(got, want) {
			// Signed by this account's webhook secret, but is it from the
			// environment of its key? A mismatch is not a bad signature: the
			// event is genuine and does not belong here.
			var ev struct {
				Livemode *bool `json:"livemode"`
			}
			_ = json.Unmarshal(req.Body, &ev)
			return s.checkLivemode(ev.Livemode)
		}
	}
	return connector.ErrInvalidSignature
}

// checkLivemode refuses an event whose livemode flag is not the environment of
// the account's secret key (sk_live_/rk_live_ or sk_test_/rk_test_), or that
// has none: a test-mode event must never book a payment of a live account,
// nor the reverse. The key is what the connector knows of its mode; a live
// account holding a test key is refused earlier, when it is loaded.
func (s *stripe) checkLivemode(livemode *bool) error {
	want := keyLivemode(s.cfg.SecretKey)
	if want == nil {
		return nil // not a key Stripe issues; nothing to compare with
	}
	name := func(live bool) string { return map[bool]string{true: "live", false: "test"}[live] }
	switch {
	case livemode == nil:
		return fmt.Errorf("stripe: event without a livemode flag refused (the account's key is a %s key)", name(*want))
	case *livemode != *want:
		return fmt.Errorf("stripe: %s-mode event refused: the account's key is a %s key", name(*livemode), name(*want))
	}
	return nil
}

// keyLivemode says which Stripe environment a secret key belongs to; nil if
// it cannot tell.
func keyLivemode(key string) *bool {
	t, f := true, false
	switch {
	case strings.HasPrefix(key, "sk_live_"), strings.HasPrefix(key, "rk_live_"):
		return &t
	case strings.HasPrefix(key, "sk_test_"), strings.HasPrefix(key, "rk_test_"):
		return &f
	}
	return nil
}

type event struct {
	ID       string `json:"id"`
	Livemode *bool  `json:"livemode"`
	Type     string `json:"type"`
	Created  int64  `json:"created"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type invoiceWire struct {
	ID            string               `json:"id"`
	Status        string               `json:"status"`
	BillingReason string               `json:"billing_reason"`
	AmountPaid    int64                `json:"amount_paid"`
	AmountDue     int64                `json:"amount_due"`
	Currency      string               `json:"currency"`
	Created       int64                `json:"created"`
	Subscription  expandable[struct{}] `json:"subscription"`
	PaymentIntent expandable[struct{}] `json:"payment_intent"`
	// SubscriptionDetails.Metadata is the subscription's metadata at the time of
	// the invoice: it carries our reference without another request.
	SubscriptionDetails struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription_details"`
	// Parent is where API versions from 2025 on put the subscription: they have no
	// top-level subscription or payment_intent on an invoice (verified against a
	// real account on 2026-09-30).
	Parent struct {
		SubscriptionDetails struct {
			Subscription expandable[struct{}] `json:"subscription"`
			Metadata     map[string]string    `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Lines struct {
		Data []struct {
			Period struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"period"`
			Metadata map[string]string `json:"metadata"`
		} `json:"data"`
	} `json:"lines"`
}

// ParseWebhook turns an event into Notifications. It makes no requests.
// Checkout sessions, refunds, disputes, invoices (renewals) and subscriptions
// are mapped; every other event type yields nothing.
func (s *stripe) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var ev event
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("stripe: bad webhook body: %w", err)
	}
	if ev.ID == "" || ev.Type == "" {
		return nil, fmt.Errorf("stripe: webhook without an event id or type")
	}
	if err := s.checkLivemode(ev.Livemode); err != nil {
		return nil, err
	}
	n := connector.Notification{EventID: ev.ID, EventType: ev.Type, OccurredAt: ms(ev.Created)}
	obj := ev.Data.Object
	switch {
	case strings.HasPrefix(ev.Type, "checkout.session."):
		var sess session
		if err := json.Unmarshal(obj, &sess); err != nil {
			return nil, fmt.Errorf("stripe: %s object: %w", ev.Type, err)
		}
		if sess.Mode == "subscription" {
			if ev.Type != "checkout.session.completed" || sess.Subscription.ID == "" {
				return nil, nil
			}
			n.Kind = connector.NotifySubscription
			n.Subscription = &connector.SubscriptionSnapshot{ProviderSubscriptionID: sess.Subscription.ID, ProviderRef: firstNonEmpty(sess.Metadata[metaRef], sess.ClientReferenceID),
				Status: "active", RawStatus: sess.Status + "/" + sess.PaymentStatus, Amount: fromStripe(sess.AmountTotal, sess.Currency), Currency: upper(sess.Currency)}
			break
		}
		snap := sess.snapshot()
		switch ev.Type {
		case "checkout.session.async_payment_failed":
			snap.Status = connector.PaymentFailed
		case "checkout.session.completed", "checkout.session.async_payment_succeeded", "checkout.session.expired":
		default:
			return nil, nil
		}
		n.Kind, n.Payment = connector.NotifyPayment, &snap
	case ev.Type == "refund.created" || ev.Type == "refund.updated" || ev.Type == "refund.failed" || ev.Type == "charge.refund.updated":
		var w refundWire
		if err := json.Unmarshal(obj, &w); err != nil {
			return nil, fmt.Errorf("stripe: %s object: %w", ev.Type, err)
		}
		snap := w.snapshot()
		n.Kind, n.Refund = connector.NotifyRefund, &snap
	case strings.HasPrefix(ev.Type, "charge.dispute."):
		var d disputeWire
		if err := json.Unmarshal(obj, &d); err != nil {
			return nil, fmt.Errorf("stripe: %s object: %w", ev.Type, err)
		}
		snap := d.snapshot()
		n.Kind, n.Dispute = connector.NotifyDispute, &snap
	case ev.Type == "invoice.paid" || ev.Type == "invoice.payment_failed":
		var inv invoiceWire
		if err := json.Unmarshal(obj, &inv); err != nil {
			return nil, fmt.Errorf("stripe: %s object: %w", ev.Type, err)
		}
		if inv.Subscription.ID == "" {
			inv.Subscription = inv.Parent.SubscriptionDetails.Subscription
			if inv.SubscriptionDetails.Metadata == nil {
				inv.SubscriptionDetails.Metadata = inv.Parent.SubscriptionDetails.Metadata
			}
		}
		if inv.Subscription.ID == "" {
			return nil, nil // not a subscription invoice
		}
		n.Kind = connector.NotifySubscription
		sub := connector.SubscriptionSnapshot{ProviderSubscriptionID: inv.Subscription.ID, ProviderRef: inv.SubscriptionDetails.Metadata[metaRef],
			Status: "active", RawStatus: inv.Status, Amount: fromStripe(inv.AmountPaid, inv.Currency), Currency: upper(inv.Currency)}
		if len(inv.Lines.Data) > 0 {
			sub.CurrentPeriodStart, sub.CurrentPeriodEnd = ms(inv.Lines.Data[0].Period.Start), ms(inv.Lines.Data[0].Period.End)
			if sub.ProviderRef == "" {
				sub.ProviderRef = inv.Lines.Data[0].Metadata[metaRef]
			}
		}
		pay := connector.PaymentSnapshot{ProviderRef: sub.ProviderRef, ProviderPaymentID: inv.PaymentIntent.ID, SubscriptionID: inv.Subscription.ID,
			Status: connector.PaymentSucceeded, RawStatus: inv.Status, Amount: fromStripe(inv.AmountPaid, inv.Currency), Currency: upper(inv.Currency),
			CreatedAt: ms(inv.Created)}
		if ev.Type == "invoice.payment_failed" {
			sub.Status, sub.Amount = "past_due", fromStripe(inv.AmountDue, inv.Currency)
			pay.Status, pay.Amount = connector.PaymentFailed, sub.Amount
		}
		n.Subscription = &sub
		if inv.PaymentIntent.ID != "" { // a free first invoice (a trial) has no payment
			n.Payment = &pay
		}
	case ev.Type == "customer.subscription.updated" || ev.Type == "customer.subscription.deleted":
		var w subscriptionWire
		if err := json.Unmarshal(obj, &w); err != nil {
			return nil, fmt.Errorf("stripe: %s object: %w", ev.Type, err)
		}
		snap := w.snapshot()
		if ev.Type == "customer.subscription.deleted" {
			snap.Status = "canceled"
		}
		n.Kind, n.Subscription = connector.NotifySubscription, &snap
	default:
		return nil, nil
	}
	if err := n.Validate(); err != nil {
		return nil, fmt.Errorf("stripe: %w", err)
	}
	return []connector.Notification{n}, nil
}
