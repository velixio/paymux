package cashfree

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// Settlements are read from POST /settlement/recon: one item per event (a
// payment, a refund, an adjustment) carrying the settlement it was paid out in,
// so the items are grouped by cf_settlement_id.
//
// Verified against the sandbox (api version 2023-08-01): the date filters are
// start_date / end_date, a window may span at most 30 days, cf_settlement_ids
// must be integers, and at least one filter is required. The item schema is
// Cashfree's published one; the sandbox account has no settlements, so a
// populated response has not been seen.
//
// TODO(spec): confirm with a real settled payment that amount_settled is the
// net payout of the whole settlement, and how event_settlement_amount relates to
// event_amount minus event_service_charge and event_service_tax.

const (
	reconPage   = 100
	maxPages    = 50
	maxWindow   = 30 * 24 * time.Hour
	defaultSpan = 30 * 24 * time.Hour
)

var ist = time.FixedZone("IST", 5*3600+30*60)

type reconItem struct {
	Event struct {
		Type             string `json:"event_type"`
		Amount           amount `json:"event_amount"`
		SettlementAmount amount `json:"event_settlement_amount"`
		ServiceCharge    amount `json:"event_service_charge"`
		ServiceTax       amount `json:"event_service_tax"`
		SaleType         string `json:"sale_type"`
		Currency         string `json:"event_currency"`
	} `json:"event_details"`
	Order struct {
		ID string `json:"order_id"`
	} `json:"order_details"`
	Payment struct {
		CFPaymentID json.Number `json:"cf_payment_id"`
	} `json:"payment_details"`
	Refunds []struct {
		RefundID string `json:"refund_id"`
	} `json:"refund_details"`
	Settlement struct {
		ID     json.Number `json:"cf_settlement_id"`
		UTR    string      `json:"settlement_utr"`
		Date   string      `json:"settlement_date"`
		Amount amount      `json:"amount_settled"`
	} `json:"settlement_details"`
}

type reconPageWire struct {
	Cursor string      `json:"cursor"`
	Data   []reconItem `json:"data"`
}

// ListSettlements returns the settlements paid out in the range, each with its
// lines. The API reads at most 30 days at a time, so a longer range is read in
// windows and merged; with no range the last 30 days are read.
func (c *cashfree) ListSettlements(ctx context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	to := c.deps.Now()
	if tr.To > 0 {
		to = time.UnixMilli(tr.To)
	}
	from := to.Add(-defaultSpan)
	if tr.From > 0 {
		from = time.UnixMilli(tr.From)
	}
	byID := map[string]*connector.SettlementSnapshot{}
	var order []string
	for start := from; start.Before(to); start = start.Add(maxWindow) {
		end := start.Add(maxWindow)
		if end.After(to) {
			end = to
		}
		if err := c.readWindow(ctx, start, end, byID, &order); err != nil {
			return nil, err
		}
	}
	out := make([]connector.SettlementSnapshot, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

func (c *cashfree) readWindow(ctx context.Context, start, end time.Time, byID map[string]*connector.SettlementSnapshot, order *[]string) error {
	filters := map[string]any{"start_date": start.In(ist).Format(time.RFC3339), "end_date": end.In(ist).Format(time.RFC3339)}
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return fmt.Errorf("cashfree: more than %d pages of settlement events", maxPages)
		}
		pg := map[string]any{"limit": reconPage}
		if cursor != "" {
			pg["cursor"] = cursor
		}
		var res reconPageWire
		if err := c.api.do(ctx, "POST", "/settlement/recon", map[string]any{"pagination": pg, "filters": filters}, &res, ""); err != nil {
			return err
		}
		for _, it := range res.Data {
			id := string(it.Settlement.ID)
			if id == "" {
				continue // not settled yet
			}
			snap, ok := byID[id]
			if !ok {
				snap = &connector.SettlementSnapshot{ProviderSettlementID: id, Currency: "INR", UTR: it.Settlement.UTR, SettledAt: parseTime(it.Settlement.Date), Status: "initiated"}
				if it.Settlement.Date != "" {
					snap.Status = "settled"
				}
				byID[id] = snap
				*order = append(*order, id)
			}
			addItem(snap, it)
		}
		if res.Cursor == "" || res.Cursor == cursor || len(res.Data) == 0 {
			return nil
		}
		cursor = res.Cursor
	}
}

// addItem adds one event to its settlement and keeps the totals: the net amount
// is the sum of the events' settled amounts (debits negative), unless the API
// states amount_settled.
func addItem(s *connector.SettlementSnapshot, r reconItem) {
	amt, _ := r.Event.Amount.minor("INR")
	fee, _ := r.Event.ServiceCharge.minor("INR")
	tax, _ := r.Event.ServiceTax.minor("INR")
	net, _ := r.Event.SettlementAmount.minor("INR")
	it := connector.SettlementItem{Amount: amt, Fee: fee, Tax: tax}
	debit := r.Event.SaleType == "DEBIT"
	switch r.Event.Type {
	case "PAYMENT":
		it.Kind, it.ProviderPaymentID = "payment", paymentID(r.Order.ID, r.Payment.CFPaymentID)
	case "REFUND", "REFUND_REVERSAL":
		it.Kind = "refund"
		for _, rf := range r.Refunds {
			if rf.RefundID != "" {
				it.ProviderRefundID = rf.RefundID
				break
			}
		}
		debit = r.Event.Type == "REFUND"
	default:
		it.Kind = "adjustment"
	}
	if debit {
		it.Amount, net = -it.Amount, -net
	}
	s.Items = append(s.Items, it)
	s.Fees += fee
	s.Tax += tax
	if stated, _ := r.Settlement.Amount.minor("INR"); r.Settlement.Amount != "" {
		s.Amount = stated
	} else {
		s.Amount += net
	}
}

var _ connector.Settler = (*cashfree)(nil)
