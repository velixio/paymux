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
	Livemode       bool              `json:"livemode"` // false for a test-mode object
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
	// The routing restrictions the payment was created with.
	AllowedConnectors []string `json:"allowed_connectors,omitempty"`
	Country           string   `json:"country,omitempty"`
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
	return PaymentView{ID: p.ID, Object: "payment", Livemode: p.Mode == "live", Status: p.Status, Kind: p.Kind, Amount: p.Amount, Currency: p.Currency,
		AmountReceived: p.AmountReceived, AmountRefunded: p.AmountRefunded, OrderRef: p.OrderRef,
		Description: p.Description, CustomerID: p.CustomerID, Metadata: meta, ReturnURL: p.ReturnURL,
		ExpiresAt: p.ExpiresAt, CreatedAt: p.CreatedAt, Version: p.Version, Flags: flags,
		AllowedConnectors: p.AllowedConnectors, Country: p.Country}
}

// RefundView is the public JSON shape of a refund.
type RefundView struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Livemode    bool   `json:"livemode"` // false for a test-mode object
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
	return RefundView{ID: r.ID, Object: "refund", Livemode: r.Mode == "live", PaymentID: r.PaymentID, AttemptID: r.AttemptID, Amount: r.Amount,
		Currency: r.Currency, Status: r.Status, Reason: r.Reason, InitiatedBy: r.InitiatedBy, ProviderRefundID: r.ProviderRefundID, CreatedAt: r.CreatedAt}
}

// paymentEventData is the data of payment.* events: the payment plus, where
// relevant, why it needs attention and which attempt caused it.
type paymentEventData struct {
	PaymentView
	Reason    string `json:"reason,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
}

// AttemptView is the public JSON shape of a payment attempt: what an app needs
// to debug a failed payment, and nothing internal.
type AttemptView struct {
	ID                string    `json:"id"`
	Status            string    `json:"status"`
	Provider          string    `json:"provider"` // the connector, for example "razorpay"
	ProviderPaymentID string    `json:"provider_payment_id,omitempty"`
	Method            string    `json:"method,omitempty"`
	FailureCode       string    `json:"failure_code,omitempty"`
	FailureMessage    string    `json:"failure_message,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}
