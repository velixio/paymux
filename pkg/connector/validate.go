package connector

import "fmt"

// Validate reports the first way n breaks the contract of ParseWebhook: it has
// an event id, an event type and a kind, carries the snapshot its kind names,
// and that snapshot identifies the provider's object (a notification without a
// provider id cannot be matched to anything). Connectors call it on what they
// are about to return; fuzz and conformance tests call it on what they got.
func (n Notification) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("notification %q: %s", n.EventType, msg) }
	if n.EventID == "" {
		return bad("empty event id")
	}
	if n.EventType == "" {
		return bad("empty event type")
	}
	switch n.Kind {
	case NotifyPayment:
		if p := n.Payment; p == nil {
			return bad("payment kind without a payment")
		} else if p.ProviderRef == "" && p.ProviderOrderID == "" && p.ProviderPaymentID == "" {
			return bad("payment without any provider id")
		}
	case NotifyRefund:
		if r := n.Refund; r == nil {
			return bad("refund kind without a refund")
		} else if r.ProviderRefundID == "" {
			return bad("refund without a provider refund id")
		}
	case NotifyDispute:
		if d := n.Dispute; d == nil {
			return bad("dispute kind without a dispute")
		} else if d.ProviderDisputeID == "" {
			return bad("dispute without a provider dispute id")
		}
	case NotifyMandate:
		if m := n.Mandate; m == nil {
			return bad("mandate kind without a mandate")
		} else if m.ProviderMandateID == "" {
			return bad("mandate without a provider mandate id")
		}
	case NotifySubscription:
		if s := n.Subscription; s == nil {
			return bad("subscription kind without a subscription")
		} else if s.ProviderSubscriptionID == "" && s.ProviderRef == "" {
			return bad("subscription without any provider id")
		}
	default:
		return bad("unknown kind " + string(n.Kind))
	}
	return nil
}
