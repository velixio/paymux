package razorpay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// Wire entities (only the fields Paymux reads). Timestamps are unix seconds.
type paymentEntity struct {
	ID            string `json:"id"`
	OrderID       string `json:"order_id"`
	Status        string `json:"status"`
	Method        string `json:"method"`
	International bool   `json:"international"`
	Bank          string `json:"bank"`
	Wallet        string `json:"wallet"`
	Card          *struct {
		Last4         string `json:"last4"`
		Network       string `json:"network"`
		Type          string `json:"type"`
		Issuer        string `json:"issuer"`
		International bool   `json:"international"`
		EMI           bool   `json:"emi"`
		SubType       string `json:"sub_type"`
	} `json:"card"`
	UPI *struct {
		PayerAccountType string `json:"payer_account_type"`
	} `json:"upi"`
	Currency string `json:"currency"`
	// SubscriptionID is set by Razorpay on payments that charge a subscription.
	SubscriptionID   string `json:"subscription_id"`
	ErrorCode        string `json:"error_code"`
	ErrorDescription string `json:"error_description"`
	Amount           int64  `json:"amount"`
	Fee              int64  `json:"fee"`
	Tax              int64  `json:"tax"`
	AmountRefunded   int64  `json:"amount_refunded"`
	CreatedAt        int64  `json:"created_at"`
}

type refundEntity struct {
	ID        string `json:"id"`
	PaymentID string `json:"payment_id"`
	Receipt   string `json:"receipt"`
	Status    string `json:"status"`
	Currency  string `json:"currency"`
	Amount    int64  `json:"amount"`
	CreatedAt int64  `json:"created_at"`
}

type disputeEntity struct {
	ID         string `json:"id"`
	PaymentID  string `json:"payment_id"`
	Status     string `json:"status"`
	Phase      string `json:"phase"`
	ReasonCode string `json:"reason_code"`
	Currency   string `json:"currency"`
	Amount     int64  `json:"amount"`
	RespondBy  int64  `json:"respond_by"`
	CreatedAt  int64  `json:"created_at"`
}

func ms(sec int64) int64 { return sec * 1000 }

// Status mapping. Unknown values map to the safe in-flight status so an
// unexpected provider state never reads as success or final failure.
func mapPaymentStatus(raw string) connector.PaymentStatus {
	switch raw {
	case "captured", "refunded": // refunds change amount_refunded, not the payment status
		return connector.PaymentSucceeded
	case "authorized":
		return connector.PaymentAuthorized
	case "failed":
		return connector.PaymentFailed
	default: // "created" and anything new
		return connector.PaymentProcessing
	}
}

func mapRefundStatus(raw string) connector.RefundStatus {
	switch raw {
	case "processed":
		return connector.RefundSucceeded
	case "failed":
		return connector.RefundFailed
	default: // "pending", "created"
		return connector.RefundPending
	}
}

func mapDisputeStatus(raw string) connector.DisputeStatus {
	switch raw {
	case "open", "action_required":
		return connector.DisputeNeedsResponse
	case "won":
		return connector.DisputeWon
	case "lost":
		return connector.DisputeLost
	case "closed":
		return connector.DisputeClosed
	default: // "under_review" and anything new
		return connector.DisputeUnderReview
	}
}

func (p paymentEntity) snapshot() connector.PaymentSnapshot {
	details := connector.PaymentMethodDetails{Bank: p.Bank, Wallet: p.Wallet}
	if p.Card != nil {
		international, emi := p.Card.International, p.Card.EMI
		details.Card = &connector.CardDetails{Last4: p.Card.Last4, Network: p.Card.Network, Funding: p.Card.Type,
			Issuer: p.Card.Issuer, International: &international, EMI: &emi, SubType: p.Card.SubType}
	}
	if p.UPI != nil && p.UPI.PayerAccountType != "" {
		details.UPI = &connector.UPIDetails{PayerAccountType: p.UPI.PayerAccountType}
	}
	return connector.PaymentSnapshot{
		ProviderOrderID: p.OrderID, ProviderPaymentID: p.ID, SubscriptionID: p.SubscriptionID,
		Status: mapPaymentStatus(p.Status), RawStatus: p.Status,
		Amount: p.Amount, Currency: p.Currency, Method: p.Method, MethodDetails: details,
		Fee: p.Fee, Tax: p.Tax, AmountRefunded: p.AmountRefunded,
		ErrorCode: p.ErrorCode, ErrorMessage: p.ErrorDescription, CreatedAt: ms(p.CreatedAt),
	}
}

func (e refundEntity) snapshot() connector.RefundSnapshot {
	return connector.RefundSnapshot{
		ProviderRef: e.Receipt, ProviderRefundID: e.ID, ProviderPaymentID: e.PaymentID,
		Status: mapRefundStatus(e.Status), RawStatus: e.Status,
		Amount: e.Amount, Currency: e.Currency, CreatedAt: ms(e.CreatedAt),
	}
}

func (d disputeEntity) snapshot() connector.DisputeSnapshot {
	return connector.DisputeSnapshot{
		ProviderDisputeID: d.ID, ProviderPaymentID: d.PaymentID,
		Status: mapDisputeStatus(d.Status), RawStatus: d.Status, Phase: d.Phase, Reason: d.ReasonCode,
		Amount: d.Amount, Currency: d.Currency, DueBy: ms(d.RespondBy), CreatedAt: ms(d.CreatedAt),
	}
}

// VerifyWebhook checks X-Razorpay-Signature, the hex HMAC-SHA256 of the raw
// body under the webhook secret, in constant time.
//
// A subscription event is applied from its own payload, so a captured delivery
// must not stay replayable for ever: its signed created_at has to be within
// subscriptionEventWindow of the time of receipt (Razorpay retries for about a
// day; the window is 48 hours either way), or it is refused as a stale signature.
// Payment, refund and dispute events are applied from a fresh fetch and need no
// such check. The X-Razorpay-Event-Id header is not covered by the signature and
// is not used (see ParseWebhook).
func (r *razorpay) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	if r.cfg.WebhookSecret == "" {
		return connector.ErrInvalidSignature
	}
	sig := req.Header("X-Razorpay-Signature")
	if sig == "" || !validHMAC(r.cfg.WebhookSecret, req.Body, sig) {
		return connector.ErrInvalidSignature
	}
	var head struct {
		Event     string `json:"event"`
		CreatedAt int64  `json:"created_at"`
	}
	if json.Unmarshal(req.Body, &head) == nil && strings.HasPrefix(head.Event, "subscription.") && req.ReceivedAt != 0 {
		if head.CreatedAt <= 0 {
			return connector.ErrInvalidSignature
		}
		age := time.UnixMilli(req.ReceivedAt).Sub(time.Unix(head.CreatedAt, 0))
		if age > subscriptionEventWindow || age < -subscriptionEventWindow {
			return connector.ErrInvalidSignature
		}
	}
	return nil
}

// subscriptionEventWindow is how far a subscription event's signed created_at
// may be from the time it is received.
const subscriptionEventWindow = 48 * time.Hour

type event struct {
	Event     string `json:"event"`
	CreatedAt int64  `json:"created_at"`
	Payload   struct {
		Payment *struct {
			Entity paymentEntity `json:"entity"`
		} `json:"payment"`
		Refund *struct {
			Entity refundEntity `json:"entity"`
		} `json:"refund"`
		Dispute *struct {
			Entity disputeEntity `json:"entity"`
		} `json:"dispute"`
		Subscription *struct {
			Entity subEntity `json:"entity"`
		} `json:"subscription"`
	} `json:"payload"`
}

// ParseWebhook turns an event into Notifications. It has no side effects:
// payment.authorized is reported as authorized, and the capture happens when
// the caller fetches the payment (FetchPayment). Events Paymux does not use
// (subscription.*, etc.) yield no notifications.
func (r *razorpay) ParseWebhook(ctx context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var ev event
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("razorpay: bad webhook body: %w", err)
	}
	// The id comes from the signed body alone. X-Razorpay-Event-Id is a header the
	// signature does not cover: a captured signed body replayed with another value
	// would have been applied again. Retries and dashboard resends of a delivery are
	// byte-identical, so the body hash dedupes them.
	sum := sha256.Sum256(req.Body)
	id := "body-" + hex.EncodeToString(sum[:16])
	n := connector.Notification{EventID: id, EventType: ev.Event, OccurredAt: ms(ev.CreatedAt)}
	if strings.HasPrefix(ev.Event, "subscription.") && ev.Payload.Subscription != nil {
		s, err := r.subSnapshot(ctx, ev.Payload.Subscription.Entity, false)
		if err != nil {
			return nil, err
		}
		n.Kind, n.Subscription = connector.NotifySubscription, &s
		if ev.Event == "subscription.charged" && ev.Payload.Payment != nil {
			// A renewal: the same notification carries the captured payment, so
			// the consumer can link it to the subscription without guessing.
			ps := ev.Payload.Payment.Entity.snapshot()
			if ps.SubscriptionID == "" {
				ps.SubscriptionID = s.ProviderSubscriptionID
			}
			n.Payment = &ps
		}
		return identified(n)
	}
	switch ev.Event {
	case "order.paid", "payment.captured", "payment.authorized", "payment.failed":
		if ev.Payload.Payment == nil {
			return nil, fmt.Errorf("razorpay: %s without payment entity", ev.Event)
		}
		s := ev.Payload.Payment.Entity.snapshot()
		if ev.Event == "order.paid" {
			// order.paid means the order is fully paid, i.e. captured.
			s.Status = connector.PaymentSucceeded
		}
		n.Kind, n.Payment = connector.NotifyPayment, &s
	case "refund.created", "refund.processed", "refund.failed":
		if ev.Payload.Refund == nil {
			return nil, fmt.Errorf("razorpay: %s without refund entity", ev.Event)
		}
		s := ev.Payload.Refund.Entity.snapshot()
		n.Kind, n.Refund = connector.NotifyRefund, &s
	case "payment.dispute.created", "payment.dispute.under_review", "payment.dispute.action_required",
		"payment.dispute.won", "payment.dispute.lost", "payment.dispute.closed":
		if ev.Payload.Dispute == nil {
			return nil, fmt.Errorf("razorpay: %s without dispute entity", ev.Event)
		}
		s := ev.Payload.Dispute.Entity.snapshot()
		n.Kind, n.Dispute = connector.NotifyDispute, &s
	default:
		return nil, nil
	}
	return identified(n)
}

// identified rejects a notification that breaks the SDK's contract, such as an
// entity without an id (found by fuzzing: an empty dispute object).
func identified(n connector.Notification) ([]connector.Notification, error) {
	if err := n.Validate(); err != nil {
		return nil, fmt.Errorf("razorpay: %w", err)
	}
	return []connector.Notification{n}, nil
}
