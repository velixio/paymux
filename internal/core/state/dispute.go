package state

import "slices"

// DisputeStatus is the state of a Dispute.
type DisputeStatus string

const (
	DisputeNeedsResponse DisputeStatus = "needs_response"
	DisputeUnderReview   DisputeStatus = "under_review"
	DisputeWon           DisputeStatus = "won"
	DisputeLost          DisputeStatus = "lost"
	DisputeAccepted      DisputeStatus = "accepted"
	DisputeClosed        DisputeStatus = "closed"
)

// AllDisputeStatuses lists every dispute status.
var AllDisputeStatuses = []DisputeStatus{DisputeNeedsResponse, DisputeUnderReview, DisputeWon, DisputeLost,
	DisputeAccepted, DisputeClosed}

// Terminal reports whether s ends a dispute (won, lost, accepted or closed).
func (s DisputeStatus) Terminal() bool {
	return s == DisputeWon || s == DisputeLost || s == DisputeAccepted || s == DisputeClosed
}

// DisputeNext decides a dispute transition (spec section 5): needs_response
// moves to under_review, and either moves to a terminal status. Repeats and
// stale (backward) reports are ignored; moving from one terminal status to
// another is rejected.
func DisputeNext(from, to DisputeStatus) Action {
	if !slices.Contains(AllDisputeStatuses, from) || !slices.Contains(AllDisputeStatuses, to) {
		return Reject
	}
	switch {
	case from == to:
		return Ignore
	case from.Terminal():
		if to.Terminal() {
			return Reject
		}
		return Ignore
	case to == DisputeNeedsResponse:
		return Ignore
	}
	return Apply
}
