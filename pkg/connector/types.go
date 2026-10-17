// Package connector is the SDK every payment provider integration implements.
// All request and response types are plain JSON-serializable structs: no
// funcs or interfaces, raw bodies are []byte and headers map[string][]string.
package connector

import (
	"encoding/json"
	"strings"
)

// Capability names an optional interface a connector implements.
type Capability string

// Optional capabilities declared in Info.Capabilities.
const (
	CapRefund              Capability = "refund"
	CapDisputeRead         Capability = "dispute_read"
	CapDisputeRespond      Capability = "dispute_respond"
	CapList                Capability = "list"
	CapSettle              Capability = "settle"
	CapCancel              Capability = "cancel"
	CapSubscriptionManager Capability = "subscription_manager"
	CapMandateManager      Capability = "mandate_manager"
)

// BillingMode is how recurring renewals are driven.
type BillingMode string

// Billing modes (spec 6.8).
const (
	ProviderManaged   BillingMode = "provider_managed"
	MerchantTriggered BillingMode = "merchant_triggered"
)

// RateLimit is a token bucket description.
type RateLimit struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
}

// MandateLimit is a per-method cap on a single mandate debit (minor units).
type MandateLimit struct {
	Method    string `json:"method"`
	Currency  string `json:"currency"`
	MaxAmount int64  `json:"max_amount"`
}

// Info describes a connector.
type Info struct {
	Name               string          `json:"name"`
	Version            string          `json:"version"`
	DisplayName        string          `json:"display_name"`
	Capabilities       []Capability    `json:"capabilities"`
	ConfigSchema       json.RawMessage `json:"config_schema"`
	Currencies         []string        `json:"currencies"`
	Methods            []string        `json:"methods"`
	MerchantOfRecord   bool            `json:"merchant_of_record"`
	SubscriptionModes  []BillingMode   `json:"subscription_modes"`
	PreDebitPerAttempt bool            `json:"pre_debit_per_attempt"`
	MandateLimits      []MandateLimit  `json:"mandate_limits"`
	// MandateSetupAmount is what a mandate registration debits when no real
	// charge is due yet (a trial): the provider's minimal or verification
	// amount, in minor units. 0 means the provider default of 100 (Rs 1).
	MandateSetupAmount int64 `json:"mandate_setup_amount,omitempty"`
	// RefundsVerificationDebit says the provider reverses that verification
	// debit by itself. When false, Paymux refunds it (initiated_by=system,
	// reason=mandate_verification).
	RefundsVerificationDebit bool      `json:"refunds_verification_debit,omitempty"`
	RateLimit                RateLimit `json:"rate_limit"`
}

// Has reports whether the capability is declared.
func (i Info) Has(c Capability) bool {
	for _, x := range i.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Customer is the buyer data a connector may prefill.
type Customer struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

// CheckoutRequest asks a connector to start a payment attempt.
type CheckoutRequest struct {
	PaymentID   string            `json:"payment_id"`
	AttemptID   string            `json:"attempt_id"`
	AppID       string            `json:"app_id"`
	ProviderRef string            `json:"provider_ref"`
	Amount      int64             `json:"amount"` // minor units
	Currency    string            `json:"currency"`
	Description string            `json:"description,omitempty"`
	Customer    Customer          `json:"customer"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// NextActionKind says how the pay page continues.
type NextActionKind string

// NextAction kinds (spec 6.2).
const (
	ActionRedirect NextActionKind = "redirect"
	ActionFormPost NextActionKind = "form_post"
	ActionSDK      NextActionKind = "sdk"
)

// NextAction is what the pay page must do next.
type NextAction struct {
	Kind NextActionKind `json:"kind"`
	// URL is the redirect target (redirect) or form action (form_post).
	URL string `json:"url,omitempty"`
	// Fields are the form_post fields.
	Fields map[string]string `json:"fields,omitempty"`
	// ScriptURL and Options are used by sdk.
	ScriptURL string         `json:"script_url,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
}

// Valid reports whether the action has the fields its kind requires.
func (n NextAction) Valid() bool {
	switch n.Kind {
	case ActionRedirect:
		return n.URL != ""
	case ActionFormPost:
		return n.URL != "" && len(n.Fields) > 0
	case ActionSDK:
		return n.ScriptURL != "" && len(n.Options) > 0
	}
	return false
}

// CheckoutResult is the outcome of CreateCheckout.
type CheckoutResult struct {
	ProviderOrderID string     `json:"provider_order_id,omitempty"`
	NextAction      NextAction `json:"next_action"`
	ExpiresAt       int64      `json:"expires_at,omitempty"` // unix ms
}

// ReturnRequest is the customer's browser coming back from the provider.
type ReturnRequest struct {
	ProviderRef string              `json:"provider_ref"`
	Method      string              `json:"method"`
	Query       map[string][]string `json:"query,omitempty"`
	Form        map[string][]string `json:"form,omitempty"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Body        []byte              `json:"body,omitempty"`
}

// Param returns the first form or query value for key.
func (r ReturnRequest) Param(key string) string {
	if v := r.Form[key]; len(v) > 0 {
		return v[0]
	}
	if v := r.Query[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// ReturnResult carries the provider ids proven by the return signature.
type ReturnResult struct {
	ProviderRef       string `json:"provider_ref"`
	ProviderOrderID   string `json:"provider_order_id,omitempty"`
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
}

// WebhookRequest is an inbound provider webhook.
type WebhookRequest struct {
	Method     string              `json:"method"`
	Headers    map[string][]string `json:"headers"`
	Query      map[string][]string `json:"query,omitempty"`
	Body       []byte              `json:"body"`
	ReceivedAt int64               `json:"received_at"` // unix ms
}

// Header returns the first value of a header, case-insensitively.
func (w WebhookRequest) Header(name string) string {
	for k, v := range w.Headers {
		if len(v) > 0 && strings.EqualFold(k, name) {
			return v[0]
		}
	}
	return ""
}

// NotificationKind classifies what a webhook is about.
type NotificationKind string

// Notification kinds.
const (
	NotifyPayment      NotificationKind = "payment"
	NotifyRefund       NotificationKind = "refund"
	NotifyDispute      NotificationKind = "dispute"
	NotifyMandate      NotificationKind = "mandate"
	NotifySubscription NotificationKind = "subscription"
)

// Notification is one normalized fact extracted from a webhook.
type Notification struct {
	Kind         NotificationKind      `json:"kind"`
	EventID      string                `json:"event_id"`
	EventType    string                `json:"event_type"` // raw provider event name
	OccurredAt   int64                 `json:"occurred_at,omitempty"`
	Payment      *PaymentSnapshot      `json:"payment,omitempty"`
	Refund       *RefundSnapshot       `json:"refund,omitempty"`
	Dispute      *DisputeSnapshot      `json:"dispute,omitempty"`
	Mandate      *MandateSnapshot      `json:"mandate,omitempty"`
	Subscription *SubscriptionSnapshot `json:"subscription,omitempty"`
}

// PaymentRef identifies a payment at the provider. At least one id must be set.
type PaymentRef struct {
	ProviderRef       string `json:"provider_ref,omitempty"`
	ProviderOrderID   string `json:"provider_order_id,omitempty"`
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
}

// PaymentStatus is the SDK-level payment status.
type PaymentStatus string

// Payment statuses.
const (
	PaymentCreated        PaymentStatus = "created"
	PaymentRequiresAction PaymentStatus = "requires_action"
	PaymentProcessing     PaymentStatus = "processing"
	PaymentAuthorized     PaymentStatus = "authorized"
	PaymentSucceeded      PaymentStatus = "succeeded"
	PaymentFailed         PaymentStatus = "failed"
	PaymentCanceled       PaymentStatus = "canceled"
	PaymentExpired        PaymentStatus = "expired"
)

// Valid reports whether s is a known status.
func (s PaymentStatus) Valid() bool {
	switch s {
	case PaymentCreated, PaymentRequiresAction, PaymentProcessing, PaymentAuthorized,
		PaymentSucceeded, PaymentFailed, PaymentCanceled, PaymentExpired:
		return true
	}
	return false
}

// PaymentSnapshot is the provider's view of a payment.
type PaymentSnapshot struct {
	ProviderRef       string `json:"provider_ref,omitempty"`
	ProviderOrderID   string `json:"provider_order_id,omitempty"`
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
	// SubscriptionID is the provider subscription a payment belongs to (a
	// provider-managed renewal), empty for one-time payments.
	SubscriptionID string        `json:"subscription_id,omitempty"`
	Status         PaymentStatus `json:"status"`
	RawStatus      string        `json:"raw_status"`
	Amount         int64         `json:"amount"`
	Currency       string        `json:"currency"`
	Method         string        `json:"method,omitempty"`
	Fee            int64         `json:"fee,omitempty"`
	Tax            int64         `json:"tax,omitempty"`
	AmountRefunded int64         `json:"amount_refunded,omitempty"`
	ErrorCode      string        `json:"error_code,omitempty"`
	ErrorMessage   string        `json:"error_message,omitempty"`
	CreatedAt      int64         `json:"created_at,omitempty"` // unix ms
}

// RefundStatus is the SDK-level refund status.
type RefundStatus string

// Refund statuses.
const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// Valid reports whether s is a known status.
func (s RefundStatus) Valid() bool {
	return s == RefundPending || s == RefundSucceeded || s == RefundFailed
}

// RefundRequest asks the provider to refund a payment.
type RefundRequest struct {
	ProviderRef       string `json:"provider_ref"` // refund idempotency token
	ProviderPaymentID string `json:"provider_payment_id"`
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	Reason            string `json:"reason,omitempty"`
}

// RefundRef identifies a refund at the provider.
type RefundRef struct {
	ProviderRef       string `json:"provider_ref,omitempty"`
	ProviderRefundID  string `json:"provider_refund_id,omitempty"`
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
}

// RefundSnapshot is the provider's view of a refund.
type RefundSnapshot struct {
	ProviderRef       string       `json:"provider_ref,omitempty"`
	ProviderRefundID  string       `json:"provider_refund_id"`
	ProviderPaymentID string       `json:"provider_payment_id"`
	Status            RefundStatus `json:"status"`
	RawStatus         string       `json:"raw_status"`
	Amount            int64        `json:"amount"`
	Currency          string       `json:"currency"`
	CreatedAt         int64        `json:"created_at,omitempty"`
}

// DisputeStatus is the SDK-level dispute status.
type DisputeStatus string

// Dispute statuses.
const (
	DisputeNeedsResponse DisputeStatus = "needs_response"
	DisputeUnderReview   DisputeStatus = "under_review"
	DisputeWon           DisputeStatus = "won"
	DisputeLost          DisputeStatus = "lost"
	DisputeAccepted      DisputeStatus = "accepted"
	DisputeClosed        DisputeStatus = "closed"
)

// Valid reports whether s is a known status.
func (s DisputeStatus) Valid() bool {
	switch s {
	case DisputeNeedsResponse, DisputeUnderReview, DisputeWon, DisputeLost, DisputeAccepted, DisputeClosed:
		return true
	}
	return false
}

// DisputeRef identifies a dispute at the provider.
type DisputeRef struct {
	ProviderDisputeID string `json:"provider_dispute_id"`
}

// DisputeSnapshot is the provider's view of a dispute.
type DisputeSnapshot struct {
	ProviderDisputeID string        `json:"provider_dispute_id"`
	ProviderPaymentID string        `json:"provider_payment_id"`
	Status            DisputeStatus `json:"status"`
	RawStatus         string        `json:"raw_status"`
	Phase             string        `json:"phase,omitempty"`
	Reason            string        `json:"reason,omitempty"`
	Amount            int64         `json:"amount"`
	Currency          string        `json:"currency"`
	DueBy             int64         `json:"due_by,omitempty"` // unix ms
	CreatedAt         int64         `json:"created_at,omitempty"`
}

// Evidence is dispute evidence to submit.
type Evidence struct {
	Text  string            `json:"text,omitempty"`
	Files map[string][]byte `json:"files,omitempty"` // type -> content
}

// SettlementSnapshot is one provider settlement.
type SettlementSnapshot struct {
	ProviderSettlementID string `json:"provider_settlement_id"`
	Amount               int64  `json:"amount"`
	Fees                 int64  `json:"fees"`
	Tax                  int64  `json:"tax"`
	Currency             string `json:"currency"`
	Status               string `json:"status"`
	UTR                  string `json:"utr,omitempty"`
	SettledAt            int64  `json:"settled_at,omitempty"`
	// Items are the lines of the settlement: per-payment fees and the GST on
	// them, which Paymux attaches to its attempts.
	Items []SettlementItem `json:"items,omitempty"`
}

// SettlementItem is one line of a settlement.
type SettlementItem struct {
	Kind              string `json:"kind"` // payment | refund | adjustment
	ProviderPaymentID string `json:"provider_payment_id,omitempty"`
	ProviderRefundID  string `json:"provider_refund_id,omitempty"`
	Amount            int64  `json:"amount"` // gross, minor units; negative for refunds
	Fee               int64  `json:"fee"`
	Tax               int64  `json:"tax"` // GST charged on the fee
}

// SubscriptionRequest creates a provider-managed subscription checkout.
type SubscriptionRequest struct {
	SubscriptionID string            `json:"subscription_id"`
	ProviderRef    string            `json:"provider_ref"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Interval       string            `json:"interval"` // day|week|month|year
	IntervalCount  int               `json:"interval_count"`
	TotalCount     int               `json:"total_count,omitempty"`
	StartAt        int64             `json:"start_at,omitempty"`
	Description    string            `json:"description,omitempty"`
	Customer       Customer          `json:"customer"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// SubscriptionRef identifies a subscription at the provider.
type SubscriptionRef struct {
	ProviderSubscriptionID string `json:"provider_subscription_id"`
}

// SubscriptionSnapshot is the provider's view of a subscription.
type SubscriptionSnapshot struct {
	ProviderSubscriptionID string `json:"provider_subscription_id"`
	// ProviderRef is the subscription's provider_ref, when the provider hands
	// it back (for example from checkout metadata). It lets a notification
	// about a subscription the provider created after the checkout (Whop
	// memberships) be linked to its Paymux subscription.
	ProviderRef        string `json:"provider_ref,omitempty"`
	Status             string `json:"status"`
	RawStatus          string `json:"raw_status"`
	Amount             int64  `json:"amount"`
	Currency           string `json:"currency"`
	CurrentPeriodStart int64  `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   int64  `json:"current_period_end,omitempty"`
}

// MandateRequest creates a merchant-triggered mandate checkout.
type MandateRequest struct {
	MandateID   string `json:"mandate_id"`
	ProviderRef string `json:"provider_ref"`
	// Amount is what the registration transaction debits (with the customer's
	// authentication): the first period's amount, or Info.MandateSetupAmount for
	// a trial. Every later debit is merchant-triggered with a prior notice.
	Amount     int64    `json:"amount"`
	MaxAmount  int64    `json:"max_amount"`
	Currency   string   `json:"currency"`
	Method     string   `json:"method,omitempty"`
	ValidUntil int64    `json:"valid_until,omitempty"`
	Customer   Customer `json:"customer"`
}

// MandateRef identifies a mandate at the provider.
type MandateRef struct {
	ProviderMandateID string `json:"provider_mandate_id"`
}

// MandateSnapshot is the provider's view of a mandate.
type MandateSnapshot struct {
	ProviderMandateID string `json:"provider_mandate_id"`
	Status            string `json:"status"` // pending|active|revoked|expired|failed
	RawStatus         string `json:"raw_status"`
	MaxAmount         int64  `json:"max_amount"`
	Currency          string `json:"currency"`
	Method            string `json:"method,omitempty"`
	ValidUntil        int64  `json:"valid_until,omitempty"`
	// SetupPayment is the registration transaction, when the provider reports
	// it with the mandate: its success is the first period's payment.
	SetupPayment *PaymentSnapshot `json:"setup_payment,omitempty"`
}

// PreDebitRequest announces an upcoming mandate debit.
type PreDebitRequest struct {
	Mandate     MandateRef `json:"mandate"`
	ProviderRef string     `json:"provider_ref"`
	Amount      int64      `json:"amount"`
	Currency    string     `json:"currency"`
	DebitDate   int64      `json:"debit_date"` // unix ms
}

// ChargeRequest debits a mandate. ProviderRef is the idempotency token.
type ChargeRequest struct {
	Mandate     MandateRef `json:"mandate"`
	ProviderRef string     `json:"provider_ref"`
	Amount      int64      `json:"amount"`
	Currency    string     `json:"currency"`
}

// TimeRange is a half-open [From, To) range in unix ms.
type TimeRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Cursor is an opaque pagination token; empty means start.
type Cursor string

// Page is one page of results.
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  Cursor `json:"next,omitempty"`
}
