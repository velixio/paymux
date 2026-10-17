package connector

import (
	"strings"
	"testing"
)

func TestNotificationValidate(t *testing.T) {
	ok := Notification{Kind: NotifyPayment, EventID: "e", EventType: "t", Payment: &PaymentSnapshot{ProviderRef: "r"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, good := range []Notification{
		{Kind: NotifyRefund, EventID: "e", EventType: "t", Refund: &RefundSnapshot{ProviderRefundID: "x"}},
		{Kind: NotifyDispute, EventID: "e", EventType: "t", Dispute: &DisputeSnapshot{ProviderDisputeID: "x"}},
		{Kind: NotifyMandate, EventID: "e", EventType: "t", Mandate: &MandateSnapshot{ProviderMandateID: "x"}},
		{Kind: NotifySubscription, EventID: "e", EventType: "t", Subscription: &SubscriptionSnapshot{ProviderRef: "x"}},
		{Kind: NotifyPayment, EventID: "e", EventType: "t", Payment: &PaymentSnapshot{ProviderOrderID: "o"}},
		{Kind: NotifyPayout, EventID: "e", EventType: "t", Payout: &PayoutSnapshot{ProviderRef: "po_r"}},
		{Kind: NotifyPayout, EventID: "e", EventType: "t", Payout: &PayoutSnapshot{ProviderPayoutID: "po_1"}},
	} {
		if err := good.Validate(); err != nil {
			t.Errorf("%s: %v", good.Kind, err)
		}
	}
	bad := map[string]Notification{
		"event id":     {Kind: NotifyPayment, EventType: "t", Payment: ok.Payment},
		"event type":   {Kind: NotifyPayment, EventID: "e", Payment: ok.Payment},
		"a payment":    {Kind: NotifyPayment, EventID: "e", EventType: "t"},
		"provider id":  {Kind: NotifyPayment, EventID: "e", EventType: "t", Payment: &PaymentSnapshot{}},
		"refund id":    {Kind: NotifyRefund, EventID: "e", EventType: "t", Refund: &RefundSnapshot{}},
		"a refund":     {Kind: NotifyRefund, EventID: "e", EventType: "t"},
		"dispute id":   {Kind: NotifyDispute, EventID: "e", EventType: "t", Dispute: &DisputeSnapshot{}},
		"mandate id":   {Kind: NotifyMandate, EventID: "e", EventType: "t", Mandate: &MandateSnapshot{}},
		"subscription": {Kind: NotifySubscription, EventID: "e", EventType: "t", Subscription: &SubscriptionSnapshot{}},
		"a payout":     {Kind: NotifyPayout, EventID: "e", EventType: "t"},
		"payout id":    {Kind: NotifyPayout, EventID: "e", EventType: "t", Payout: &PayoutSnapshot{}},
		"unknown kind": {Kind: "x", EventID: "e", EventType: "t"},
	}
	for want, n := range bad {
		if err := n.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
