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
	err := CheckNotifications([]connector.Notification{ok, {Kind: connector.NotifyRefund, EventID: "e", EventType: "t", Refund: &connector.RefundSnapshot{}}})
	if err == nil || !strings.Contains(err.Error(), "result 1") || !strings.Contains(err.Error(), "refund") {
		t.Errorf("err = %v", err)
	}
}
