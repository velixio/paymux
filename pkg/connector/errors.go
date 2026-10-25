package connector

import (
	"errors"
	"fmt"
	"time"
)

// Typed errors (spec 9, rule 3).
var (
	ErrNotFound           = errors.New("connector: not found")
	ErrInvalidCredentials = errors.New("connector: invalid credentials")
	ErrTemporary          = errors.New("connector: temporary failure")
	ErrUnsupported        = errors.New("connector: unsupported")
	// ErrRejected means the provider answered that the request is invalid or not
	// allowed (its 400, 409 and 422-style answers: "currency not supported",
	// "only offers can be updated", a refused amount). The error text carries the
	// provider's own message. It is the caller's to fix, not an outage to retry and
	// not a Paymux fault: the API shows it as a 4xx (provider_rejected) with the
	// message, redacted and bounded. A connector returns it after the specific
	// errors (not found, invalid credentials, rate limited, not refundable,
	// unsupported); 5xx answers and transport errors stay ErrTemporary.
	ErrRejected = errors.New("connector: rejected by the provider")
	// ErrInvalidSignature is returned by VerifyWebhook when the signature is
	// missing, wrong or stale (timestamp outside the tolerance). The webhook is
	// rejected with 401, stored as unverified and never causes a fetch.
	ErrInvalidSignature = errors.New("connector: webhook signature missing, wrong or stale")
	// ErrUnsigned is returned by VerifyWebhook only for event types the
	// provider genuinely does not sign (today: PayU refund webhooks). Such an
	// event is stored unverified and enqueues a fetch-only job: the fetched
	// API result, not the webhook, is what may change state.
	ErrUnsigned = errors.New("connector: webhook event type is not signed by the provider")
	// ErrInvalidReturn is returned by HandleReturn when the return signature fails.
	ErrInvalidReturn = errors.New("connector: return signature invalid")
	ErrMandateLimit  = errors.New("connector: amount exceeds mandate limit")
)

// ErrNotRefundable says the provider refuses the refund.
type ErrNotRefundable struct{ Reason string }

func (e ErrNotRefundable) Error() string { return "connector: not refundable: " + e.Reason }

// ErrRateLimited says to retry after RetryAfter.
type ErrRateLimited struct{ RetryAfter time.Duration }

func (e ErrRateLimited) Error() string {
	return fmt.Sprintf("connector: rate limited, retry after %s", e.RetryAfter)
}
