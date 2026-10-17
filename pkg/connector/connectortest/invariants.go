package connectortest

import (
	"fmt"

	"github.com/paymux/paymux/pkg/connector"
)

// CheckNotifications reports the first way a ParseWebhook result breaks the
// SDK's contract, for fuzz targets and table tests: every notification has an
// event id, an event type and a kind, carries the snapshot its kind names, and
// that snapshot identifies the provider object (a notification without a
// provider id cannot be matched to anything).
func CheckNotifications(ns []connector.Notification) error {
	for i, n := range ns {
		at := func(msg string) error { return fmt.Errorf("notification %d (%s): %s", i, n.EventType, msg) }
		if n.EventID == "" {
			return at("empty event id")
		}
		if n.EventType == "" {
			return at("empty event type")
		}
		switch n.Kind {
		case connector.NotifyPayment:
			if p := n.Payment; p == nil {
				return at("payment kind without a payment")
			} else if p.ProviderRef == "" && p.ProviderOrderID == "" && p.ProviderPaymentID == "" {
				return at("payment without any provider id")
			}
		case connector.NotifyRefund:
			if r := n.Refund; r == nil {
				return at("refund kind without a refund")
			} else if r.ProviderRefundID == "" {
				return at("refund without a provider refund id")
			}
		case connector.NotifyDispute:
			if d := n.Dispute; d == nil {
				return at("dispute kind without a dispute")
			} else if d.ProviderDisputeID == "" {
				return at("dispute without a provider dispute id")
			}
		case connector.NotifyMandate:
			if m := n.Mandate; m == nil {
				return at("mandate kind without a mandate")
			} else if m.ProviderMandateID == "" {
				return at("mandate without a provider mandate id")
			}
		case connector.NotifySubscription:
			if s := n.Subscription; s == nil {
				return at("subscription kind without a subscription")
			} else if s.ProviderSubscriptionID == "" && s.ProviderRef == "" {
				return at("subscription without any provider id")
			}
		default:
			return at("unknown kind " + string(n.Kind))
		}
	}
	return nil
}
