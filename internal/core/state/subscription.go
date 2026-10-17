package state

import "slices"

// SubscriptionStatus is the lifecycle state of a Subscription (spec §5).
type SubscriptionStatus string

const (
	SubIncomplete        SubscriptionStatus = "incomplete"
	SubIncompleteExpired SubscriptionStatus = "incomplete_expired"
	SubTrialing          SubscriptionStatus = "trialing"
	SubActive            SubscriptionStatus = "active"
	SubPastDue           SubscriptionStatus = "past_due"
	SubCanceled          SubscriptionStatus = "canceled"
	SubEnded             SubscriptionStatus = "ended"
)

// AllSubscriptionStatuses lists every subscription status.
var AllSubscriptionStatuses = []SubscriptionStatus{SubIncomplete, SubIncompleteExpired, SubTrialing, SubActive, SubPastDue, SubCanceled, SubEnded}

// SubscriptionEvent drives subscription transitions.
type SubscriptionEvent string

const (
	SubSetupSucceeded         SubscriptionEvent = "setup_succeeded"
	SubSetupSucceededTrial    SubscriptionEvent = "setup_succeeded_trial"
	SubSetupExpired           SubscriptionEvent = "setup_expired"
	SubRenewalSucceeded       SubscriptionEvent = "renewal_succeeded"
	SubRenewalFailed          SubscriptionEvent = "renewal_failed"
	SubDunningExhaustedCancel SubscriptionEvent = "dunning_exhausted_cancel"
	SubDunningExhaustedKeep   SubscriptionEvent = "dunning_exhausted_keep"
	SubCancel                 SubscriptionEvent = "cancel"
	SubPeriodEndCancel        SubscriptionEvent = "period_end_cancel"
	SubCompleted              SubscriptionEvent = "completed"
)

// AllSubscriptionEvents lists every subscription event.
var AllSubscriptionEvents = []SubscriptionEvent{SubSetupSucceeded, SubSetupSucceededTrial, SubSetupExpired, SubRenewalSucceeded,
	SubRenewalFailed, SubDunningExhaustedCancel, SubDunningExhaustedKeep, SubCancel, SubPeriodEndCancel, SubCompleted}

// SubscriptionOutcome is the result of SubscriptionNext.
type SubscriptionOutcome struct {
	Action Action
	Next   SubscriptionStatus
}

func validSub(s SubscriptionStatus) bool { return slices.Contains(AllSubscriptionStatuses, s) }

// SubscriptionNext returns the transition for ev in state from. Terminal
// states (canceled, ended, incomplete_expired) ignore everything; a renewal
// success arriving after cancel is handled at the payment level (needs_review).
func SubscriptionNext(from SubscriptionStatus, ev SubscriptionEvent) SubscriptionOutcome {
	if !validSub(from) || !slices.Contains(AllSubscriptionEvents, ev) {
		return SubscriptionOutcome{Action: Reject, Next: from}
	}
	ignore := SubscriptionOutcome{Action: Ignore, Next: from}
	to := func(s SubscriptionStatus) SubscriptionOutcome { return SubscriptionOutcome{Action: Apply, Next: s} }
	switch from {
	case SubIncomplete:
		switch ev {
		case SubSetupSucceeded:
			return to(SubActive)
		case SubSetupSucceededTrial:
			return to(SubTrialing)
		case SubSetupExpired:
			return to(SubIncompleteExpired)
		case SubCancel:
			return to(SubCanceled)
		case SubRenewalSucceeded, SubRenewalFailed:
			return SubscriptionOutcome{Action: Reject, Next: from}
		}
	case SubTrialing, SubActive, SubPastDue:
		switch ev {
		case SubRenewalSucceeded:
			return to(SubActive)
		case SubRenewalFailed:
			return to(SubPastDue)
		case SubCancel, SubPeriodEndCancel:
			return to(SubCanceled)
		case SubCompleted:
			return to(SubEnded)
		case SubDunningExhaustedCancel:
			if from == SubPastDue {
				return to(SubCanceled)
			}
		}
	}
	return ignore
}

// MandateStatus is the lifecycle state of a Mandate.
type MandateStatus string

const (
	MandatePending MandateStatus = "pending"
	MandateActive  MandateStatus = "active"
	MandateRevoked MandateStatus = "revoked"
	MandateExpired MandateStatus = "expired"
	MandateFailed  MandateStatus = "failed"
)

// MandateEvent drives mandate transitions.
type MandateEvent string

const (
	MandateActivated MandateEvent = "activated"
	MandateFailedEv  MandateEvent = "failed"
	MandateRevokedEv MandateEvent = "revoked"
	MandateExpiredEv MandateEvent = "expired"
)

// MandateOutcome is the result of MandateNext.
type MandateOutcome struct {
	Action Action
	Next   MandateStatus
}

// MandateNext returns the mandate transition; revoked, expired and failed are terminal.
func MandateNext(from MandateStatus, ev MandateEvent) MandateOutcome {
	to := func(s MandateStatus) MandateOutcome { return MandateOutcome{Action: Apply, Next: s} }
	switch from {
	case MandatePending:
		switch ev {
		case MandateActivated:
			return to(MandateActive)
		case MandateFailedEv:
			return to(MandateFailed)
		case MandateRevokedEv:
			return to(MandateRevoked)
		case MandateExpiredEv:
			return to(MandateExpired)
		}
	case MandateActive:
		switch ev {
		case MandateRevokedEv:
			return to(MandateRevoked)
		case MandateExpiredEv:
			return to(MandateExpired)
		}
	case MandateRevoked, MandateExpired, MandateFailed:
	default:
		return MandateOutcome{Action: Reject, Next: from}
	}
	return MandateOutcome{Action: Ignore, Next: from}
}
