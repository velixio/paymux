package stripe

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/velixio/paymux/pkg/connector"
)

// Settlements are Stripe payouts. A payout's lines are the balance
// transactions it settles, with the fee and the tax on the fee of each.
//
// Verified live: the payout list accepts the arrival_date filter, the
// balance_transactions payout filter exists, fee_details gives the fee
// (type stripe_fee) and expanding data.source gives the charge with its
// PaymentIntent. The test account has no payout bank account, so no payout
// could be made.
//
// TODO(spec): fee_details entries of type "tax" (seen only on accounts that pay
// GST on fees) and payout.trace_id.value as the UTR were not observed.

type payoutWire struct {
	ID          string `json:"id"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	ArrivalDate int64  `json:"arrival_date"`
	Created     int64  `json:"created"`
	TraceID     struct {
		Value string `json:"value"`
	} `json:"trace_id"`
}

type sourceWire struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	PaymentIntent string `json:"payment_intent"`
}

type balanceTxn struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Amount     int64                  `json:"amount"`
	Fee        int64                  `json:"fee"`
	Currency   string                 `json:"currency"`
	Source     expandable[sourceWire] `json:"source"`
	FeeDetails []struct {
		Amount int64  `json:"amount"`
		Type   string `json:"type"`
	} `json:"fee_details"`
}

// item turns a balance transaction into a settlement line.
func (b balanceTxn) item() (connector.SettlementItem, bool) {
	var taxFee int64
	for _, d := range b.FeeDetails {
		if d.Type == "tax" {
			taxFee += d.Amount
		}
	}
	it := connector.SettlementItem{Amount: fromStripe(b.Amount, b.Currency), Fee: fromStripe(b.Fee-taxFee, b.Currency), Tax: fromStripe(taxFee, b.Currency)}
	switch b.Type {
	case "payout", "payout_cancel", "payout_failure":
		return it, false // the payout itself is the settlement, not a line of it
	case "charge", "payment":
		it.Kind = "payment"
		it.ProviderPaymentID = b.Source.ID
		if b.Source.Obj != nil && b.Source.Obj.PaymentIntent != "" {
			it.ProviderPaymentID = b.Source.Obj.PaymentIntent
		}
	case "refund", "payment_refund":
		it.Kind, it.ProviderRefundID = "refund", b.Source.ID
	default:
		it.Kind = "adjustment"
	}
	return it, true
}

// ListSettlements returns the payouts that arrive in the range, each with its
// per-transaction lines.
func (s *stripe) ListSettlements(ctx context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	var payouts []payoutWire
	for cur := ""; ; {
		q := listQuery(tr, connector.Cursor(cur), "arrival_date")
		var l list[payoutWire]
		if err := s.api.do(ctx, "GET", "/v1/payouts", q, nil, &l, ""); err != nil {
			return nil, err
		}
		payouts = append(payouts, l.Data...)
		if !l.HasMore || len(l.Data) == 0 {
			break
		}
		cur = l.Data[len(l.Data)-1].ID
	}
	out := make([]connector.SettlementSnapshot, 0, len(payouts))
	for _, p := range payouts {
		snap := connector.SettlementSnapshot{ProviderSettlementID: p.ID, Amount: fromStripe(p.Amount, p.Currency), Currency: upper(p.Currency),
			Status: p.Status, UTR: p.TraceID.Value, SettledAt: ms(p.ArrivalDate)}
		for cur := ""; ; {
			q := url.Values{"payout": {p.ID}, "limit": {strconv.Itoa(listPageSize)}, "expand[]": {"data.source"}}
			if cur != "" {
				q.Set("starting_after", cur)
			}
			var l list[balanceTxn]
			if err := s.api.do(ctx, "GET", "/v1/balance_transactions", q, nil, &l, ""); err != nil {
				return nil, fmt.Errorf("stripe: transactions of payout %s: %w", p.ID, err)
			}
			for _, b := range l.Data {
				if it, ok := b.item(); ok {
					snap.Items = append(snap.Items, it)
					snap.Fees += it.Fee
					snap.Tax += it.Tax
				}
			}
			if !l.HasMore || len(l.Data) == 0 {
				break
			}
			cur = l.Data[len(l.Data)-1].ID
		}
		out = append(out, snap)
	}
	return out, nil
}

var _ connector.Settler = (*stripe)(nil)
