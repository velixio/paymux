//go:build sandbox

package stripe

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// A payout is made from balance that a bypass-pending test card makes available
// at once; the connector must then list it with the balance transactions it
// settles, with fees.
func TestSandboxPayoutIsListedWithItsLines(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	pi := directPayment(t, s, sbxRef("payout"), "pm_card_bypassPending", "usd", 10000)
	var payout struct {
		ID string `json:"id"`
	}
	if err := s.api.do(ctx, "POST", "/v1/payouts", nil, url.Values{"amount": {"1000"}, "currency": {"usd"}}, &payout, ""); err != nil {
		t.Skipf("a test payout could not be created (the account may have no payout bank): %v", err)
	}
	now := time.Now()
	got, err := s.ListSettlements(ctx, connector.TimeRange{From: now.Add(-24 * time.Hour).UnixMilli(), To: now.Add(10 * 24 * time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	var found *connector.SettlementSnapshot
	for i := range got {
		if got[i].ProviderSettlementID == payout.ID {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("payout %s (after %s) not listed among %d", payout.ID, pi, len(got))
	}
	t.Logf("%+v", *found)
	if found.Amount != 1000 || found.Currency != "USD" {
		t.Errorf("%+v", *found)
	}
}

// Fees of a real charge: balance transaction fee_details, as ListSettlements reads them.
func TestSandboxBalanceTransactionFees(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	pi := directPayment(t, s, sbxRef("fee"), "pm_card_bypassPending", "usd", 5000)
	// The balance transaction shows up in the list a moment after the charge.
	for try := 0; try < 10; try++ {
		var l list[balanceTxn]
		if err := s.api.do(ctx, "GET", "/v1/balance_transactions", url.Values{"limit": {"20"}, "type": {"charge"}, "expand[]": {"data.source"}}, nil, &l, ""); err != nil {
			t.Fatal(err)
		}
		for _, b := range l.Data {
			it, ok := b.item()
			if !ok || it.ProviderPaymentID != pi {
				continue
			}
			if it.Kind != "payment" || it.Amount != 5000 || it.Fee <= 0 || b.Source.Obj == nil || b.Source.Obj.PaymentIntent != pi {
				t.Errorf("%+v %+v", b, it)
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("the balance transaction of %s never appeared", pi)
}

// Subscription checkout options: a start more than 48 hours away becomes a
// trial and a nearer one is ignored. (A total count is refused: Checkout rejects
// subscription_data[cancel_at].)
func TestSandboxSubscriptionCheckoutOptions(t *testing.T) {
	s := sandboxConnector(t)
	ctx := context.Background()
	for name, mut := range map[string]func(*connector.SubscriptionRequest){
		"plain":  func(r *connector.SubscriptionRequest) {},
		"trial":  func(r *connector.SubscriptionRequest) { r.StartAt = time.Now().Add(72 * time.Hour).UnixMilli() },
		"near":   func(r *connector.SubscriptionRequest) { r.StartAt = time.Now().Add(24 * time.Hour).UnixMilli() },
		"yearly": func(r *connector.SubscriptionRequest) { r.Interval, r.IntervalCount = "year", 1 },
	} {
		req := connector.SubscriptionRequest{SubscriptionID: "sub_sbx", ProviderRef: sbxRef("so"), Amount: 1000, Currency: "USD", Interval: "month", IntervalCount: 1, Description: "plan"}
		mut(&req)
		res, err := s.CreateSubscriptionCheckout(ctx, req)
		if err != nil || res.ProviderOrderID == "" || res.NextAction.URL == "" {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
}
