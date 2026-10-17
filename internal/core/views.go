package core

import (
	"time"

	"github.com/paymux/paymux/internal/store"
)

// PaymentView is the public JSON shape of a payment, used by the API and as
// the snapshot inside events. The client secret hash is never included.
type PaymentView struct {
	ID             string            `json:"id"`
	Object         string            `json:"object"`
	Status         string            `json:"status"`
	Kind           string            `json:"kind"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	AmountReceived int64             `json:"amount_received"`
	AmountRefunded int64             `json:"amount_refunded"`
	OrderRef       string            `json:"order_ref,omitempty"`
	Description    string            `json:"description,omitempty"`
	CustomerID     string            `json:"customer_id,omitempty"`
	Metadata       map[string]string `json:"metadata"`
	ReturnURL      string            `json:"return_url,omitempty"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	Version        int64             `json:"version"`
	Flags          []string          `json:"flags"`
}

// ViewPayment renders p.
func ViewPayment(p store.Payment) PaymentView {
	meta, flags := p.Metadata, p.Flags
	if meta == nil {
		meta = map[string]string{}
	}
	if flags == nil {
		flags = []string{}
	}
	return PaymentView{ID: p.ID, Object: "payment", Status: p.Status, Kind: p.Kind, Amount: p.Amount, Currency: p.Currency,
		AmountReceived: p.AmountReceived, AmountRefunded: p.AmountRefunded, OrderRef: p.OrderRef,
		Description: p.Description, CustomerID: p.CustomerID, Metadata: meta, ReturnURL: p.ReturnURL,
		ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt, Version: p.Version, Flags: flags}
}

// RefundView is the public JSON shape of a refund.
type RefundView struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	PaymentID   string `json:"payment_id"`
	AttemptID   string `json:"attempt_id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	InitiatedBy string `json:"initiated_by"`
	// ProviderRefundID is the provider's id for the refund, once the provider
	// has accepted it. Absent while the refund is still queued here.
	ProviderRefundID string    `json:"provider_refund_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ViewRefund renders r.
func ViewRefund(r store.Refund) RefundView {
	return RefundView{ID: r.ID, Object: "refund", PaymentID: r.PaymentID, AttemptID: r.AttemptID, Amount: r.Amount,
		Currency: r.Currency, Status: r.Status, Reason: r.Reason, InitiatedBy: r.InitiatedBy, ProviderRefundID: r.ProviderRefundID, CreatedAt: r.CreatedAt}
}

// paymentEventData is the data of payment.* events: the payment plus, where
// relevant, why it needs attention and which attempt caused it.
type paymentEventData struct {
	PaymentView
	Reason    string `json:"reason,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}
