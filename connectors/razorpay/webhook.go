package razorpay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
)

// Wire entities (only the fields Paymux reads). Timestamps are unix seconds.
type paymentEntity struct {
	ID               string `json:"id"`
	OrderID          string `json:"order_id"`
	Status           string `json:"status"`
	Method           string `json:"method"`
	Currency         string `json:"currency"`
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
	return connector.PaymentSnapshot{
		ProviderOrderID: p.OrderID, ProviderPaymentID: p.ID,
		Status: mapPaymentStatus(p.Status), RawStatus: p.Status,
		Amount: p.Amount, Currency: p.Currency, Method: p.Method,
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
func (r *razorpay) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	sig := req.Header("X-Razorpay-Signature")
	if sig == "" || !validHMAC(r.cfg.WebhookSecret, req.Body, sig) {
		return connector.ErrInvalidSignature
	}
	return nil
}

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
	id := req.Header("X-Razorpay-Event-Id")
	if id == "" {
		// Retries of the same delivery are byte-identical, so a body hash dedupes.
		sum := sha256.Sum256(req.Body)
		id = "body-" + hex.EncodeToString(sum[:16])
	}
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
			n.Payment = &ps
		}
		return []connector.Notification{n}, nil
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
	return []connector.Notification{n}, nil
}
