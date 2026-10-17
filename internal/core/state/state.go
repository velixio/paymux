// Package state holds the pure transition tables for Payment, Attempt and
// Refund (spec section 5). Nothing here touches a database or a clock.
package state

import "slices"

// Action is what the caller must do with a requested transition.
type Action int

const (
	// Ignore means the request is stale or redundant: change nothing.
	Ignore Action = iota
	// Apply means move to the outcome's Next status.
	Apply
	// Duplicate means a second success on an already-succeeded payment. The
	// status stays; the caller flags the payment and applies the
	// duplicate_payment_action policy.
	Duplicate
	// Reject means the request is invalid in this state (for example cancel
	// after success) and should surface as an error.
	Reject
)

func (a Action) String() string {
	switch a {
	case Ignore:
		return "ignore"
	case Apply:
		return "apply"
	case Duplicate:
		return "duplicate"
	case Reject:
		return "reject"
	}
	return "unknown"
}

// PaymentStatus is the lifecycle state of a Payment.
type PaymentStatus string

const (
	PaymentCreated        PaymentStatus = "created"
	PaymentRequiresAction PaymentStatus = "requires_action"
	PaymentProcessing     PaymentStatus = "processing"
	PaymentSucceeded      PaymentStatus = "succeeded"
	PaymentFailed         PaymentStatus = "failed"
	PaymentExpired        PaymentStatus = "expired"
	PaymentCanceled       PaymentStatus = "canceled"
	PaymentNeedsReview    PaymentStatus = "needs_review"
)

// AllPaymentStatuses lists every payment status.
var AllPaymentStatuses = []PaymentStatus{PaymentCreated, PaymentRequiresAction, PaymentProcessing, PaymentSucceeded,
	PaymentFailed, PaymentExpired, PaymentCanceled, PaymentNeedsReview}

// PaymentEvent is something that happened to a payment.
type PaymentEvent string

const (
	EvRequiresAction PaymentEvent = "requires_action"
	EvProcessing     PaymentEvent = "processing"
	// EvSucceeded is a success whose amount and currency match the payment.
	EvSucceeded PaymentEvent = "succeeded"
	// EvSucceededMismatch is a success whose amount or currency differs.
	EvSucceededMismatch PaymentEvent = "succeeded_mismatch"
	EvFailed            PaymentEvent = "failed"
	EvExpired           PaymentEvent = "expired"
	EvCancel            PaymentEvent = "cancel"
)

// AllPaymentEvents lists every payment event.
var AllPaymentEvents = []PaymentEvent{EvRequiresAction, EvProcessing, EvSucceeded, EvSucceededMismatch, EvFailed,
	EvExpired, EvCancel}

// Reasons attached to a move into needs_review.
const (
	ReasonAmountMismatch     = "amount_mismatch"
	ReasonSuccessAfterCancel = "success_after_cancel"
)

// PaymentOutcome is the result of PaymentNext.
type PaymentOutcome struct {
	Action Action
	// Next is the status after the transition; equal to the current status
	// unless Action is Apply.
	Next PaymentStatus
	// Late marks a success that arrived after failed, expired or canceled.
	Late bool
	// Reason is set when Next is needs_review.
	Reason string
}

// PaymentNext applies ev to a payment in status from.
//
// Rules (spec section 5): forward-only; the one backward move is to
// succeeded from failed or expired (late success). Success after cancel and
// any amount or currency mismatch go to needs_review. A success on a
// succeeded payment is a Duplicate. Unknown inputs are rejected.
func PaymentNext(from PaymentStatus, ev PaymentEvent) PaymentOutcome {
	if !slices.Contains(AllPaymentStatuses, from) || !slices.Contains(AllPaymentEvents, ev) {
		return PaymentOutcome{Action: Reject, Next: from}
	}
	ignore := PaymentOutcome{Action: Ignore, Next: from}
	apply := func(to PaymentStatus) PaymentOutcome { return PaymentOutcome{Action: Apply, Next: to} }
	review := func(reason string, late bool) PaymentOutcome {
		return PaymentOutcome{Action: Apply, Next: PaymentNeedsReview, Late: late, Reason: reason}
	}

	if ev == EvCancel {
		switch from {
		case PaymentCreated, PaymentRequiresAction:
			return apply(PaymentCanceled)
		case PaymentCanceled:
			return ignore
		}
		return PaymentOutcome{Action: Reject, Next: from}
	}

	switch from {
	case PaymentCreated, PaymentRequiresAction, PaymentProcessing:
		switch ev {
		case EvRequiresAction:
			if from == PaymentCreated {
				return apply(PaymentRequiresAction)
			}
		case EvProcessing:
			if from != PaymentProcessing {
				return apply(PaymentProcessing)
			}
		case EvSucceeded:
			return apply(PaymentSucceeded)
		case EvSucceededMismatch:
			return review(ReasonAmountMismatch, false)
		case EvFailed:
			return apply(PaymentFailed)
		case EvExpired:
			// A payment the provider is already working on is settled by
			// the poller, not by the clock.
			if from != PaymentProcessing {
				return apply(PaymentExpired)
			}
		}
	case PaymentFailed, PaymentExpired:
		switch ev {
		case EvSucceeded:
			return PaymentOutcome{Action: Apply, Next: PaymentSucceeded, Late: true}
		case EvSucceededMismatch:
			return review(ReasonAmountMismatch, true)
		}
	case PaymentCanceled:
		if ev == EvSucceeded || ev == EvSucceededMismatch {
			return review(ReasonSuccessAfterCancel, true)
		}
	case PaymentSucceeded:
		if ev == EvSucceeded || ev == EvSucceededMismatch {
			return PaymentOutcome{Action: Duplicate, Next: PaymentSucceeded}
		}
	case PaymentNeedsReview:
		// Operators resolve a review; the machine does not move it.
	}
	return ignore
}

// AttemptStatus is the state of one PaymentAttempt.
type AttemptStatus string

const (
	AttemptCreated    AttemptStatus = "created"
	AttemptPending    AttemptStatus = "pending"
	AttemptAuthorized AttemptStatus = "authorized"
	AttemptCaptured   AttemptStatus = "captured"
	AttemptFailed     AttemptStatus = "failed"
)

// AllAttemptStatuses lists every attempt status.
var AllAttemptStatuses = []AttemptStatus{AttemptCreated, AttemptPending, AttemptAuthorized, AttemptCaptured, AttemptFailed}

// AttemptOutcome is the result of AttemptNext.
type AttemptOutcome struct {
	Action Action // Ignore, Apply or Reject
	// Late marks a capture that arrives after the attempt was marked failed.
	Late bool
}

var attemptRank = map[AttemptStatus]int{AttemptCreated: 0, AttemptPending: 1, AttemptAuthorized: 2}

// AttemptNext decides whether an attempt in status from may move to status
// to. Attempts are monotonic (created, pending, authorized, captured) with
// failed as an exit from any open state. captured is terminal. The one
// backward move is failed to captured (late success).
func AttemptNext(from, to AttemptStatus) AttemptOutcome {
	if !slices.Contains(AllAttemptStatuses, from) || !slices.Contains(AllAttemptStatuses, to) {
		return AttemptOutcome{Action: Reject}
	}
	switch from {
	case AttemptCaptured:
		return AttemptOutcome{Action: Ignore}
	case AttemptFailed:
		if to == AttemptCaptured {
			return AttemptOutcome{Action: Apply, Late: true}
		}
		return AttemptOutcome{Action: Ignore}
	}
	switch to {
	case AttemptCaptured, AttemptFailed:
		return AttemptOutcome{Action: Apply}
	}
	if attemptRank[to] > attemptRank[from] {
		return AttemptOutcome{Action: Apply}
	}
	return AttemptOutcome{Action: Ignore}
}

// RefundStatus is the state of a Refund.
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
)

// AllRefundStatuses lists every refund status.
var AllRefundStatuses = []RefundStatus{RefundPending, RefundSucceeded, RefundFailed}

// RefundNext decides a refund transition: pending moves to succeeded or
// failed. Repeats and stale pending reports are ignored; flipping one
// terminal state to the other is rejected.
func RefundNext(from, to RefundStatus) Action {
	if !slices.Contains(AllRefundStatuses, from) || !slices.Contains(AllRefundStatuses, to) {
		return Reject
	}
	switch {
	case from == to || to == RefundPending:
		return Ignore
	case from == RefundPending:
		return Apply
	}
	return Reject
}
