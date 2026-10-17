package connector

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Connector is the required interface.
type Connector interface {
	Info() Info
	CreateCheckout(ctx context.Context, req CheckoutRequest) (CheckoutResult, error)
	HandleReturn(ctx context.Context, req ReturnRequest) (ReturnResult, error)
	VerifyWebhook(ctx context.Context, req WebhookRequest) error
	ParseWebhook(ctx context.Context, req WebhookRequest) ([]Notification, error)
	FetchPayment(ctx context.Context, ref PaymentRef) (PaymentSnapshot, error)
}

// Refunder is optional (CapRefund).
type Refunder interface {
	Refund(ctx context.Context, req RefundRequest) (RefundSnapshot, error)
	FetchRefund(ctx context.Context, ref RefundRef) (RefundSnapshot, error)
}

// DisputeReader is optional (CapDisputeRead).
type DisputeReader interface {
	ListDisputes(ctx context.Context, tr TimeRange, cur Cursor) (Page[DisputeSnapshot], error)
	FetchDispute(ctx context.Context, ref DisputeRef) (DisputeSnapshot, error)
}

// DisputeResponder is optional (CapDisputeRespond).
type DisputeResponder interface {
	AcceptDispute(ctx context.Context, ref DisputeRef) error
	SubmitEvidence(ctx context.Context, ref DisputeRef, ev Evidence) error
}

// Lister is optional (CapList).
type Lister interface {
	ListPayments(ctx context.Context, tr TimeRange, cur Cursor) (Page[PaymentSnapshot], error)
	ListRefunds(ctx context.Context, tr TimeRange, cur Cursor) (Page[RefundSnapshot], error)
}

// Settler is optional (CapSettle).
type Settler interface {
	ListSettlements(ctx context.Context, tr TimeRange) ([]SettlementSnapshot, error)
}

// Canceler is optional (CapCancel).
type Canceler interface {
	CancelCheckout(ctx context.Context, ref PaymentRef) error
}

// SubscriptionManager is optional, for provider_managed billing
// (CapSubscriptionManager). Razorpay and Whop implement it.
type SubscriptionManager interface {
	CreateSubscriptionCheckout(ctx context.Context, req SubscriptionRequest) (CheckoutResult, error)
	FetchSubscription(ctx context.Context, ref SubscriptionRef) (SubscriptionSnapshot, error)
	CancelSubscription(ctx context.Context, ref SubscriptionRef, atPeriodEnd bool) error
	UpdateSubscriptionAmount(ctx context.Context, ref SubscriptionRef, amount int64) error
}

// MandateManager is optional, for merchant_triggered billing
// (CapMandateManager; PayU and Zoho). ChargeMandate must pass
// ChargeRequest.ProviderRef to the
// provider as the idempotency token.
type MandateManager interface {
	CreateMandateCheckout(ctx context.Context, req MandateRequest) (CheckoutResult, error)
	FetchMandate(ctx context.Context, ref MandateRef) (MandateSnapshot, error)
	NotifyPreDebit(ctx context.Context, req PreDebitRequest) error
	ChargeMandate(ctx context.Context, req ChargeRequest) (PaymentSnapshot, error)
	RevokeMandate(ctx context.Context, ref MandateRef) error
}

// Payouter is optional, for sending money out (CapPayout). CreatePayout must
// pass PayoutRequest.ProviderRef to the provider as the idempotency token: the
// same request repeated returns the same payout and never pays twice. A
// request the provider cannot take (a bad beneficiary, an amount over a limit)
// is an error; one it has taken is a snapshot, whatever its status.
type Payouter interface {
	CreatePayout(ctx context.Context, req PayoutRequest) (PayoutSnapshot, error)
	FetchPayout(ctx context.Context, ref PayoutRef) (PayoutSnapshot, error)
}

// URLs builds the URLs a connector hands to the provider.
type URLs interface {
	// ReturnURL is the customer return URL for an attempt's provider_ref.
	ReturnURL(providerRef string) string
	// WebhookURL is the inbound webhook URL for this account.
	WebhookURL() string
}

// Deps is everything a connector may use besides its config.
type Deps struct {
	HTTP   *http.Client
	KV     KV
	Clock  func() time.Time
	Logger *slog.Logger
	URLs   URLs
}

// Now returns the injected clock time, or the wall clock if unset.
func (d Deps) Now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

// Factory builds a connector from its account config.
type Factory func(cfg json.RawMessage, deps Deps) (Connector, error)
