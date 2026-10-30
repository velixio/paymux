package paymux

import (
	"encoding/json"
	"time"
)

type PaymentStatus string

const (
	PaymentStatusCreated        PaymentStatus = "created"
	PaymentStatusRequiresAction PaymentStatus = "requires_action"
	PaymentStatusProcessing     PaymentStatus = "processing"
	PaymentStatusSucceeded      PaymentStatus = "succeeded"
	PaymentStatusFailed         PaymentStatus = "failed"
	PaymentStatusExpired        PaymentStatus = "expired"
	PaymentStatusCanceled       PaymentStatus = "canceled"
	PaymentStatusNeedsReview    PaymentStatus = "needs_review"
)

type SubscriptionStatus string

const (
	SubscriptionStatusIncomplete        SubscriptionStatus = "incomplete"
	SubscriptionStatusIncompleteExpired SubscriptionStatus = "incomplete_expired"
	SubscriptionStatusTrialing          SubscriptionStatus = "trialing"
	SubscriptionStatusActive            SubscriptionStatus = "active"
	SubscriptionStatusPastDue           SubscriptionStatus = "past_due"
	SubscriptionStatusCanceled          SubscriptionStatus = "canceled"
	SubscriptionStatusEnded             SubscriptionStatus = "ended"
)

type PayoutStatus string

const (
	PayoutStatusPending     PayoutStatus = "pending"
	PayoutStatusProcessing  PayoutStatus = "processing"
	PayoutStatusSucceeded   PayoutStatus = "succeeded"
	PayoutStatusFailed      PayoutStatus = "failed"
	PayoutStatusReversed    PayoutStatus = "reversed"
	PayoutStatusNeedsReview PayoutStatus = "needs_review"
)

type Interval string

const (
	IntervalDay   Interval = "day"
	IntervalWeek  Interval = "week"
	IntervalMonth Interval = "month"
	IntervalYear  Interval = "year"
)

type PaymentMethod string

const (
	PaymentMethodUPI      PaymentMethod = "upi"
	PaymentMethodCard     PaymentMethod = "card"
	PaymentMethodEMandate PaymentMethod = "emandate"
)

type ProrationMode string

const (
	ProrationNone        ProrationMode = "none"
	ProrationNextInvoice ProrationMode = "next_invoice"
	ProrationImmediate   ProrationMode = "immediate"
)

type Customer struct {
	ExternalRef string          `json:"external_ref,omitempty"`
	Name        string          `json:"name,omitempty"`
	Email       string          `json:"email,omitempty"`
	Phone       string          `json:"phone,omitempty"`
	Billing     *BillingAddress `json:"billing,omitempty"`
}

type BillingAddress struct {
	LegalName  string `json:"legal_name,omitempty"`
	GSTIN      string `json:"gstin,omitempty"`
	Line1      string `json:"line1,omitempty"`
	Line2      string `json:"line2,omitempty"`
	City       string `json:"city,omitempty"`
	StateCode  string `json:"state_code,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

type CreatePaymentParams struct {
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	OrderRef          string            `json:"order_ref,omitempty"`
	Description       string            `json:"description,omitempty"`
	Customer          *Customer         `json:"customer,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	ReturnURL         string            `json:"return_url,omitempty"`
	AllowedConnectors []string          `json:"allowed_connectors,omitempty"`
	Country           string            `json:"country,omitempty"`
}

type Payment struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	LiveMode          bool              `json:"livemode"`
	Status            PaymentStatus     `json:"status"`
	Kind              string            `json:"kind"`
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	AmountReceived    int64             `json:"amount_received"`
	AmountRefunded    int64             `json:"amount_refunded"`
	OrderRef          string            `json:"order_ref,omitempty"`
	Description       string            `json:"description,omitempty"`
	CustomerID        string            `json:"customer_id,omitempty"`
	Metadata          map[string]string `json:"metadata"`
	ReturnURL         string            `json:"return_url,omitempty"`
	AllowedConnectors []string          `json:"allowed_connectors,omitempty"`
	Country           string            `json:"country,omitempty"`
	ExpiresAt         time.Time         `json:"expires_at"`
	CreatedAt         time.Time         `json:"created_at"`
	Version           int64             `json:"version"`
	Flags             []string          `json:"flags"`
	CheckoutMode      string            `json:"checkout_mode,omitempty"`
	CheckoutURL       string            `json:"checkout_url,omitempty"`
	Checkout          *CheckoutAction   `json:"checkout,omitempty"`
	Attempts          []PaymentAttempt  `json:"attempts,omitempty"`
}

type CheckoutActionParams struct {
	Fresh   bool             `json:"fresh,omitempty"`
	Billing *CheckoutBilling `json:"billing,omitempty"`
}

type CheckoutBilling struct {
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Country    string `json:"country,omitempty"`
	StateCode  string `json:"state_code,omitempty"`
	Line1      string `json:"line1,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	GSTIN      string `json:"gstin,omitempty"`
	LegalName  string `json:"legal_name,omitempty"`
}

type ProviderAction struct {
	Kind      string            `json:"kind"`
	URL       string            `json:"url,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	ScriptURL string            `json:"script_url,omitempty"`
	Options   map[string]any    `json:"options,omitempty"`
}

type CheckoutAction struct {
	Kind      string          `json:"kind"`
	PaymentID string          `json:"payment_id"`
	Status    PaymentStatus   `json:"status"`
	Launcher  string          `json:"launcher,omitempty"`
	Action    *ProviderAction `json:"action,omitempty"`
}

type PaymentAttempt struct {
	ID                string    `json:"id"`
	Status            string    `json:"status"`
	Provider          string    `json:"provider"`
	ProviderPaymentID string    `json:"provider_payment_id,omitempty"`
	Method            string    `json:"method,omitempty"`
	FailureCode       string    `json:"failure_code,omitempty"`
	FailureMessage    string    `json:"failure_message,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type ListPaymentsParams struct {
	Limit         int
	StartingAfter string
	Status        PaymentStatus
}
type PaymentPage struct {
	Object     string    `json:"object"`
	Data       []Payment `json:"data"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type CreateRefundParams struct {
	Amount    int64  `json:"amount,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type Refund struct {
	ID               string    `json:"id"`
	Object           string    `json:"object"`
	LiveMode         bool      `json:"livemode"`
	PaymentID        string    `json:"payment_id"`
	AttemptID        string    `json:"attempt_id"`
	Amount           int64     `json:"amount"`
	Currency         string    `json:"currency"`
	Status           string    `json:"status"`
	Reason           string    `json:"reason,omitempty"`
	InitiatedBy      string    `json:"initiated_by"`
	ProviderRefundID string    `json:"provider_refund_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type CreatePaymentLinkParams struct {
	Amount      int64             `json:"amount,omitempty"`
	MinAmount   int64             `json:"min_amount,omitempty"`
	MaxAmount   int64             `json:"max_amount,omitempty"`
	Currency    string            `json:"currency"`
	Description string            `json:"description,omitempty"`
	SingleUse   bool              `json:"single_use,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	ReturnURL   string            `json:"return_url,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type PaymentLink struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	URL               string            `json:"url"`
	Slug              string            `json:"slug"`
	AppID             string            `json:"app_id"`
	LiveMode          bool              `json:"livemode"`
	Amount            *int64            `json:"amount"`
	MinAmount         int64             `json:"min_amount,omitempty"`
	MaxAmount         int64             `json:"max_amount,omitempty"`
	Currency          string            `json:"currency"`
	Description       string            `json:"description,omitempty"`
	ReturnURL         string            `json:"return_url,omitempty"`
	SingleUse         bool              `json:"single_use"`
	Active            bool              `json:"active"`
	ExpiresAt         *time.Time        `json:"expires_at"`
	Metadata          map[string]string `json:"metadata"`
	DeactivatedAt     *time.Time        `json:"deactivated_at,omitempty"`
	DeactivatedReason string            `json:"deactivated_reason,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	Version           int64             `json:"version"`
}

type ListPaymentLinksParams struct {
	Limit         int
	StartingAfter string
	Active        *bool
}
type PaymentLinkPage struct {
	Object     string        `json:"object"`
	Data       []PaymentLink `json:"data"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type CreateSubscriptionParams struct {
	Customer          Customer          `json:"customer"`
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	Interval          Interval          `json:"interval"`
	IntervalCount     int               `json:"interval_count,omitempty"`
	TrialDays         int               `json:"trial_days,omitempty"`
	StartAt           *time.Time        `json:"start_at,omitempty"`
	TotalCount        int               `json:"total_count,omitempty"`
	Description       string            `json:"description,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	ReturnURL         string            `json:"return_url,omitempty"`
	Method            PaymentMethod     `json:"method,omitempty"`
	AllowedConnectors []string          `json:"allowed_connectors,omitempty"`
	Country           string            `json:"country,omitempty"`
}

type Subscription struct {
	ID                      string             `json:"id"`
	Object                  string             `json:"object"`
	Status                  SubscriptionStatus `json:"status"`
	BillingMode             string             `json:"billing_mode"`
	CustomerID              string             `json:"customer_id"`
	LiveMode                bool               `json:"livemode"`
	Amount                  int64              `json:"amount"`
	Currency                string             `json:"currency"`
	Interval                Interval           `json:"interval"`
	IntervalCount           int                `json:"interval_count"`
	CurrentPeriodStart      time.Time          `json:"current_period_start"`
	CurrentPeriodEnd        time.Time          `json:"current_period_end"`
	TrialEnd                *time.Time         `json:"trial_end,omitempty"`
	TotalCount              int                `json:"total_count"`
	RenewalsDone            int                `json:"renewals_done"`
	CancelAtPeriodEnd       bool               `json:"cancel_at_period_end"`
	CanceledAt              *time.Time         `json:"canceled_at,omitempty"`
	EndedAt                 *time.Time         `json:"ended_at,omitempty"`
	PendingAmount           int64              `json:"pending_amount,omitempty"`
	ProrationAdjust         int64              `json:"proration_adjust,omitempty"`
	RequiresReauthorization bool               `json:"requires_reauthorization"`
	Description             string             `json:"description,omitempty"`
	Metadata                map[string]string  `json:"metadata"`
	AllowedConnectors       []string           `json:"allowed_connectors,omitempty"`
	Country                 string             `json:"country,omitempty"`
	ReturnURL               string             `json:"return_url,omitempty"`
	MandateID               string             `json:"mandate_id,omitempty"`
	CreatedAt               time.Time          `json:"created_at"`
	Version                 int64              `json:"version"`
	CheckoutURL             string             `json:"checkout_url,omitempty"`
	Proration               *ProrationOutcome  `json:"proration,omitempty"`
}

type ProrationOutcome struct {
	Requested   string     `json:"requested"`
	Mode        string     `json:"mode"`
	Amount      int64      `json:"amount"`
	Description string     `json:"description,omitempty"`
	ChargeAt    *time.Time `json:"charge_at,omitempty"`
	Reason      string     `json:"reason,omitempty"`
}
type ListSubscriptionsParams struct {
	Limit                   int
	StartingAfter, Customer string
	Status                  SubscriptionStatus
}
type SubscriptionPage struct {
	Object     string         `json:"object"`
	Data       []Subscription `json:"data"`
	HasMore    bool           `json:"has_more"`
	NextCursor string         `json:"next_cursor,omitempty"`
}
type CancelSubscriptionParams struct {
	AtPeriodEnd bool `json:"at_period_end,omitempty"`
}
type UpdateSubscriptionParams struct {
	Amount    int64         `json:"amount"`
	Proration ProrationMode `json:"proration,omitempty"`
}
type UpdatePaymentMethodParams struct {
	Method PaymentMethod `json:"method,omitempty"`
}

type Invoice struct {
	ID              string            `json:"id"`
	Object          string            `json:"object"`
	Number          string            `json:"number"`
	PaymentID       string            `json:"payment_id"`
	SubscriptionID  string            `json:"subscription_id,omitempty"`
	LiveMode        bool              `json:"livemode"`
	IssuedAt        time.Time         `json:"issued_at"`
	SupplyType      string            `json:"supply_type"`
	PlaceOfSupply   string            `json:"place_of_supply"`
	Currency        string            `json:"currency"`
	Supplier        json.RawMessage   `json:"supplier,omitempty"`
	Recipient       json.RawMessage   `json:"recipient,omitempty"`
	Lines           []json.RawMessage `json:"lines,omitempty"`
	Taxable         int64             `json:"taxable"`
	CGST            int64             `json:"cgst"`
	SGST            int64             `json:"sgst"`
	IGST            int64             `json:"igst"`
	Total           int64             `json:"total"`
	FXRateINR       string            `json:"fx_rate_inr,omitempty"`
	Flags           []string          `json:"flags"`
	TemplateVersion string            `json:"template_version"`
	PDFReady        bool              `json:"pdf_ready"`
	IRN             string            `json:"irn,omitempty"`
	AckNo           string            `json:"ack_no,omitempty"`
	AckDate         *time.Time        `json:"ack_date,omitempty"`
}
type InvoicePage struct {
	Object  string    `json:"object"`
	Data    []Invoice `json:"data"`
	HasMore bool      `json:"has_more"`
}

type CreditNote struct {
	ID              string            `json:"id"`
	Object          string            `json:"object"`
	Number          string            `json:"number"`
	InvoiceID       string            `json:"invoice_id"`
	PaymentID       string            `json:"payment_id"`
	RefundID        string            `json:"refund_id,omitempty"`
	DisputeID       string            `json:"dispute_id,omitempty"`
	LiveMode        bool              `json:"livemode"`
	IssuedAt        time.Time         `json:"issued_at"`
	Reason          string            `json:"reason"`
	Currency        string            `json:"currency"`
	Lines           []json.RawMessage `json:"lines,omitempty"`
	Taxable         int64             `json:"taxable"`
	CGST            int64             `json:"cgst"`
	SGST            int64             `json:"sgst"`
	IGST            int64             `json:"igst"`
	Total           int64             `json:"total"`
	Flags           []string          `json:"flags"`
	TemplateVersion string            `json:"template_version"`
	PDFReady        bool              `json:"pdf_ready"`
}
type CreditNotePage struct {
	Object  string       `json:"object"`
	Data    []CreditNote `json:"data"`
	HasMore bool         `json:"has_more"`
}

type Beneficiary struct {
	Name          string `json:"name"`
	AccountNumber string `json:"account_number,omitempty"`
	IFSC          string `json:"ifsc,omitempty"`
	UPI           string `json:"upi,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
}
type CreatePayoutParams struct {
	Amount            int64             `json:"amount"`
	Currency          string            `json:"currency"`
	Beneficiary       Beneficiary       `json:"beneficiary"`
	Narration         string            `json:"narration,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	ProviderAccountID string            `json:"provider_account_id,omitempty"`
	Force             bool              `json:"force,omitempty"`
}
type Payout struct {
	ID          string       `json:"id"`
	Object      string       `json:"object"`
	Status      PayoutStatus `json:"status"`
	LiveMode    bool         `json:"livemode"`
	Amount      int64        `json:"amount"`
	Currency    string       `json:"currency"`
	Beneficiary struct {
		Label string `json:"label"`
	} `json:"beneficiary"`
	Narration      string            `json:"narration,omitempty"`
	Metadata       map[string]string `json:"metadata"`
	FailureCode    string            `json:"failure_code,omitempty"`
	FailureMessage string            `json:"failure_message,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
	Version        int64             `json:"version"`
}
type ListPayoutsParams struct {
	Limit         int
	StartingAfter string
	Status        PayoutStatus
}
type PayoutPage struct {
	Object     string   `json:"object"`
	Data       []Payout `json:"data"`
	HasMore    bool     `json:"has_more"`
	NextCursor string   `json:"next_cursor,omitempty"`
}
