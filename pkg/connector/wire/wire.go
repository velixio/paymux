// Package wire is the protocol between Paymux and an out-of-process connector
// (Path 2): the same connector.Connector methods, forwarded over HTTP.
//
// Every method is `POST {base}/{method}` with the JSON encoding of its request
// type as the body and the JSON encoding of its result as the 200 response;
// `GET {base}/info` returns the connector's Info. Bodies are signed in both
// directions with the Standard Webhooks scheme (headers webhook-id,
// webhook-timestamp, webhook-signature: v1,<base64 HMAC-SHA256(id.ts.body)>)
// under a shared `whsec_` secret. A failed call answers with a signed Error
// body, which the client turns back into the SDK's typed error.
//
// Handler serves any connector.Connector this way; connectors/remote is the
// client.
package wire

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Method names, the last path segment of each call.
const (
	MethodInfo                       = "info" // GET
	MethodCreateCheckout             = "create_checkout"
	MethodHandleReturn               = "handle_return"
	MethodVerifyWebhook              = "verify_webhook"
	MethodParseWebhook               = "parse_webhook"
	MethodFetchPayment               = "fetch_payment"
	MethodRefund                     = "refund"
	MethodFetchRefund                = "fetch_refund"
	MethodListDisputes               = "list_disputes"
	MethodFetchDispute               = "fetch_dispute"
	MethodAcceptDispute              = "accept_dispute"
	MethodSubmitEvidence             = "submit_evidence"
	MethodListPayments               = "list_payments"
	MethodListRefunds                = "list_refunds"
	MethodListSettlements            = "list_settlements"
	MethodCancelCheckout             = "cancel_checkout"
	MethodCreateSubscriptionCheckout = "create_subscription_checkout"
	MethodFetchSubscription          = "fetch_subscription"
	MethodCancelSubscription         = "cancel_subscription"
	MethodUpdateSubscriptionAmount   = "update_subscription_amount"
	MethodCreateMandateCheckout      = "create_mandate_checkout"
	MethodFetchMandate               = "fetch_mandate"
	MethodNotifyPreDebit             = "notify_pre_debit"
	MethodChargeMandate              = "charge_mandate"
	MethodRevokeMandate              = "revoke_mandate"
	MethodCreatePayout               = "create_payout"
	MethodFetchPayout                = "fetch_payout"
)

// Request envelopes for methods with more than one argument. Methods with a
// single argument send that argument's type directly.

// ListRequest is the body of list_disputes, list_payments and list_refunds.
type ListRequest struct {
	Range  connector.TimeRange `json:"range"`
	Cursor connector.Cursor    `json:"cursor,omitempty"`
}

// SettlementsRequest is the body of list_settlements.
type SettlementsRequest struct {
	Range connector.TimeRange `json:"range"`
}

// EvidenceRequest is the body of submit_evidence.
type EvidenceRequest struct {
	Dispute  connector.DisputeRef `json:"dispute"`
	Evidence connector.Evidence   `json:"evidence"`
}

// CancelSubscriptionRequest is the body of cancel_subscription.
type CancelSubscriptionRequest struct {
	Subscription connector.SubscriptionRef `json:"subscription"`
	AtPeriodEnd  bool                      `json:"at_period_end"`
}

// UpdateAmountRequest is the body of update_subscription_amount.
type UpdateAmountRequest struct {
	Subscription connector.SubscriptionRef `json:"subscription"`
	Amount       int64                     `json:"amount"`
}

// Empty is the body of calls that return nothing.
type Empty struct{}

// Notifications is the result of parse_webhook.
type Notifications struct {
	Items []connector.Notification `json:"items"`
}

// Settlements is the result of list_settlements.
type Settlements struct {
	Items []connector.SettlementSnapshot `json:"items"`
}

// Error codes of an Error body.
const (
	CodeNotFound           = "not_found"
	CodeInvalidCredentials = "invalid_credentials"
	CodeTemporary          = "temporary"
	CodeUnsupported        = "unsupported"
	CodeInvalidSignature   = "invalid_signature"
	CodeUnsigned           = "unsigned"
	CodeInvalidReturn      = "invalid_return"
	CodeMandateLimit       = "mandate_limit"
	CodeNotRefundable      = "not_refundable"
	CodeRateLimited        = "rate_limited"
	CodeInternal           = "internal"
	CodeBadRequest         = "bad_request"
)

// Error is the body of a failed call: {"error": {...}}.
type Error struct {
	Code         string `json:"code"`
	Message      string `json:"message,omitempty"`
	Reason       string `json:"reason,omitempty"`         // not_refundable
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"` // rate_limited
}

// Envelope wraps Error as the JSON body.
type Envelope struct {
	Error Error `json:"error"`
}

// FromError turns a connector error into its wire form and HTTP status.
// Anything that is not one of the SDK's typed errors is an internal error.
func FromError(err error) (int, Error) {
	e := Error{Message: err.Error()}
	var nr connector.ErrNotRefundable
	var rl connector.ErrRateLimited
	switch {
	case errors.As(err, &nr):
		e.Code, e.Reason = CodeNotRefundable, nr.Reason
	case errors.As(err, &rl):
		e.Code, e.RetryAfterMS = CodeRateLimited, rl.RetryAfter.Milliseconds()
	case errors.Is(err, connector.ErrNotFound):
		e.Code = CodeNotFound
	case errors.Is(err, connector.ErrInvalidCredentials):
		e.Code = CodeInvalidCredentials
	case errors.Is(err, connector.ErrTemporary):
		e.Code = CodeTemporary
	case errors.Is(err, connector.ErrUnsupported):
		e.Code = CodeUnsupported
	case errors.Is(err, connector.ErrInvalidSignature):
		e.Code = CodeInvalidSignature
	case errors.Is(err, connector.ErrUnsigned):
		e.Code = CodeUnsigned
	case errors.Is(err, connector.ErrInvalidReturn):
		e.Code = CodeInvalidReturn
	case errors.Is(err, connector.ErrMandateLimit):
		e.Code = CodeMandateLimit
	default:
		e.Code = CodeInternal
	}
	switch e.Code {
	case CodeRateLimited:
		return http.StatusTooManyRequests, e
	case CodeTemporary:
		return http.StatusServiceUnavailable, e
	case CodeInternal:
		return http.StatusInternalServerError, e
	}
	return http.StatusUnprocessableEntity, e
}

// Err turns a wire error back into the SDK's typed error, so that errors.Is
// and errors.As behave exactly as if the connector were in-process.
func (e Error) Err() error {
	wrap := func(base error) error {
		if e.Message == "" {
			return base
		}
		return fmt.Errorf("%w: remote: %s", base, e.Message)
	}
	switch e.Code {
	case CodeNotFound:
		return wrap(connector.ErrNotFound)
	case CodeInvalidCredentials:
		return wrap(connector.ErrInvalidCredentials)
	case CodeTemporary, CodeInternal:
		return wrap(connector.ErrTemporary)
	case CodeUnsupported:
		return wrap(connector.ErrUnsupported)
	case CodeInvalidSignature:
		return wrap(connector.ErrInvalidSignature)
	case CodeUnsigned:
		return wrap(connector.ErrUnsigned)
	case CodeInvalidReturn:
		return wrap(connector.ErrInvalidReturn)
	case CodeMandateLimit:
		return wrap(connector.ErrMandateLimit)
	case CodeNotRefundable:
		return connector.ErrNotRefundable{Reason: e.Reason}
	case CodeRateLimited:
		return connector.ErrRateLimited{RetryAfter: time.Duration(e.RetryAfterMS) * time.Millisecond}
	}
	if e.Code == CodeBadRequest {
		return fmt.Errorf("remote: bad request: %s", e.Message) // a bug in the caller: not retryable
	}
	return fmt.Errorf("%w: remote error %q: %s", connector.ErrTemporary, e.Code, e.Message)
}
