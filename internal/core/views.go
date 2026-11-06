package core

import (
	"encoding/json"
	"time"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
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
	// FailureCode and FailureMessage are set on payment.failed only.
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

// AttemptView is the public JSON shape of a payment attempt: what an app needs
// to debug a failed payment, and nothing internal.
type AttemptView struct {
	ID                string                          `json:"id"`
	Status            string                          `json:"status"`
	Provider          string                          `json:"provider"` // the connector, for example "razorpay"
	ProviderPaymentID string                          `json:"provider_payment_id,omitempty"`
	Method            string                          `json:"method,omitempty"`
	MethodDetails     *connector.PaymentMethodDetails `json:"method_details,omitempty"`
	FailureCode       string                          `json:"failure_code,omitempty"`
	FailureMessage    string                          `json:"failure_message,omitempty"`
	CreatedAt         time.Time                       `json:"created_at"`
	// Tries is every go at paying the provider reported, oldest first. The
	// attempt's own fields are its current state; history lives here.
	Tries []TryView `json:"tries"`
}

// TryView is the public JSON shape of one provider try. A value the provider
// did not report is left out.
type TryView struct {
	ProviderPaymentID string                          `json:"provider_payment_id,omitempty"`
	Status            string                          `json:"status"`
	Method            string                          `json:"method,omitempty"`
	MethodDetails     *connector.PaymentMethodDetails `json:"method_details,omitempty"`
	ThreeDSecure      *ThreeDSecureView               `json:"three_d_secure,omitempty"`
	FailureCode       string                          `json:"failure_code,omitempty"`
	FailureMessage    string                          `json:"failure_message,omitempty"`
	CreatedAt         time.Time                       `json:"created_at"`
}

// ThreeDSecureView is the outcome of 3D Secure on a try, never its data.
type ThreeDSecureView struct {
	Status string `json:"status"`
}

func viewTry(p store.ProviderPaymentAttempt) TryView {
	v := TryView{ProviderPaymentID: p.ProviderPaymentID, Status: string(state.TryStatus(p.Status)), Method: p.Method,
		MethodDetails: methodDetails(p.MethodDetails), FailureCode: p.FailureCode, FailureMessage: p.FailureMessage, CreatedAt: p.CreatedAt}
	if p.ThreeDS != "" {
		v.ThreeDSecure = &ThreeDSecureView{Status: p.ThreeDS}
	}
	return v
}

func methodDetails(raw string) *connector.PaymentMethodDetails {
	var details connector.PaymentMethodDetails
	if raw == "" || json.Unmarshal([]byte(raw), &details) != nil || details.Empty() {
		return nil
	}
	return &details
}
