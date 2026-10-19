package connector

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
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

// WebhookEventIDer is optional. The inbound handler needs a webhook's event id
// on every delivery, before anything is stored, and it must do so without
// calling the provider (a flood of forged deliveries would otherwise use up the
// account's rate limit with the provider). A connector whose ParseWebhook can
// reach the provider for some events (PayU confirms an unsigned mandate notice by
// reading the mandate back) implements this to name the event from the request
// alone: the same id ParseWebhook reports, derived deterministically from the
// body. ok is false for a delivery the connector does not name this way, and the
// caller then parses it.
type WebhookEventIDer interface {
	WebhookEventID(req WebhookRequest) (id string, ok bool)
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

// ConnectionTester is optional. The dashboard's "Test" button asks it whether
// the account's credentials work, with a real, authenticated, read-only call
// that changes nothing and needs only the permissions the connector already
// uses. Without it Paymux asks FetchPayment about a payment that cannot exist
// and takes ErrNotFound as "the credentials were accepted", which only works
// for a connector that can look a payment up by Paymux's reference alone;
// Stripe, Zoho and Whop cannot (they would fail before any request is made), so
// they implement this.
//
// TestConnection returns nil when the provider accepted the credentials, an
// error wrapping ErrInvalidCredentials when it rejected them, and any other
// error when the result is inconclusive (network, rate limit, outage). The text
// of an ErrInvalidCredentials error is shown to the operator, so it should say
// what is wrong and what to do: "not connected yet", "this token is for the
// sandbox, not for the live host".
type ConnectionTester interface {
	TestConnection(ctx context.Context) error
}

// OAuthConnector is optional, for providers whose credentials come from an
// OAuth authorization (CapOAuth; Zoho Payments). It lets the dashboard offer a
// "Connect" button: the operator registers the redirect URI with the provider,
// pastes the client id and secret, is sent to the provider to approve, and the
// refresh token that comes back is stored in the account's config, so nobody
// copies a grant code by hand.
//
// The methods are called on an instance built from the account's current
// config, which may lack the refresh token (an account that is not connected
// yet must still be constructible; its API calls then fail with
// ErrInvalidCredentials saying it is not connected). cfg is that config, with
// the client id and secret the operator just entered. A connector must never
// call a host that came from outside: where the provider names its own
// accounts server on the callback (OAuthCallbackConfig), only hosts of the
// provider's published list are accepted.
type OAuthConnector interface {
	// OAuthDefaultScopes are the scopes to ask for when the operator chooses none.
	OAuthDefaultScopes(cfg json.RawMessage) []string
	// OAuthAuthorizeURL is the provider's authorization page for the client in
	// cfg, returning to redirectURI with state.
	OAuthAuthorizeURL(cfg json.RawMessage, redirectURI, state string, scopes []string) (string, error)
	// OAuthCallbackConfig returns cfg adjusted to what the provider reported on
	// the callback (the data centre the account lives in, say), or an error when
	// that is not acceptable. params are the callback's query parameters.
	OAuthCallbackConfig(cfg json.RawMessage, params url.Values) (json.RawMessage, error)
	// OAuthExchange trades a grant code for tokens. An empty redirectURI is a
	// code from a self client (no redirect). It returns an error, and never a
	// partial result, when the provider does not return a refresh token.
	OAuthExchange(ctx context.Context, cfg json.RawMessage, redirectURI, code string) (OAuthResult, error)
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
