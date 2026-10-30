package razorpay

import (
	"context"
	"errors"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// FetchPayment by order id (the hinted-order path) must not stamp the caller's
// ProviderRef onto an order whose receipt is another reference. The fake's
// order_Nx0 has receipt "pay_ref_01"; the caller claims "pay_ref_OTHER".
func TestAudit2FetchPaymentByOrderIgnoresReceipt(t *testing.T) {
	c := newConn(t, newFake("captured"))
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "order_Nx0", ProviderRef: "pay_ref_OTHER"})
	if !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("want ErrNotFound for an order of another receipt, got snap=%+v err=%v", snap, err)
	}
}
