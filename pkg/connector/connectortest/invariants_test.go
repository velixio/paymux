package connectortest

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

func TestCheckNotifications(t *testing.T) {
	ok := connector.Notification{Kind: connector.NotifyPayment, EventID: "e", EventType: "t", Payment: &connector.PaymentSnapshot{ProviderRef: "r"}}
	if err := CheckNotifications([]connector.Notification{ok}); err != nil {
		t.Fatal(err)
	}
	if err := CheckNotifications(nil); err != nil {
		t.Fatal(err)
	}
	bad := map[string]connector.Notification{
		"event id":     {Kind: connector.NotifyPayment, EventType: "t", Payment: ok.Payment},
		"event type":   {Kind: connector.NotifyPayment, EventID: "e", Payment: ok.Payment},
		"a payment":    {Kind: connector.NotifyPayment, EventID: "e", EventType: "t"},
		"provider id":  {Kind: connector.NotifyPayment, EventID: "e", EventType: "t", Payment: &connector.PaymentSnapshot{}},
		"refund id":    {Kind: connector.NotifyRefund, EventID: "e", EventType: "t", Refund: &connector.RefundSnapshot{}},
		"dispute id":   {Kind: connector.NotifyDispute, EventID: "e", EventType: "t", Dispute: &connector.DisputeSnapshot{}},
		"mandate id":   {Kind: connector.NotifyMandate, EventID: "e", EventType: "t", Mandate: &connector.MandateSnapshot{}},
		"subscription": {Kind: connector.NotifySubscription, EventID: "e", EventType: "t", Subscription: &connector.SubscriptionSnapshot{}},
		"unknown kind": {Kind: "x", EventID: "e", EventType: "t"},
	}
	for want, n := range bad {
		if err := CheckNotifications([]connector.Notification{ok, n}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
