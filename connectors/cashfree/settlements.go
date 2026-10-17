package cashfree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Settlements come from the PG settlements API: POST /settlements lists them
// and POST /settlement/recon lists the lines of one.
//
// TODO(spec): the whole of this file is from documentation and unverified:
// the request filters (start_date, end_date, cf_settlement_ids), the cursor
// pagination, the field names (amount_settled, service_charge, event_type,
// event_service_charge, event_service_tax) and the status strings. Re-check
// each against the sandbox before relying on it.

const (
	settlementPage = 100
	maxPages       = 50
)

type settlementWire struct {
	CFSettlementID json.Number `json:"cf_settlement_id"`
	UTR            string      `json:"settlement_utr"`
	Status         string      `json:"status"`
	Amount         amount      `json:"amount_settled"`
	Date           string      `json:"settlement_date"`
	ChargesTotal   amount      `json:"service_charge"`
	TaxTotal       amount      `json:"service_tax"`
}

type reconWire struct {
	EventType string      `json:"event_type"`
	OrderID   string      `json:"order_id"`
	CFPayment json.Number `json:"cf_payment_id"`
	RefundID  string      `json:"refund_id"`
	Amount    amount      `json:"event_amount"`
	Charge    amount      `json:"event_service_charge"`
	Tax       amount      `json:"event_service_tax"`
}

type pageOf[T any] struct {
	Cursor string `json:"cursor"`
	Data   []T    `json:"data"`
}

// ListSettlements returns the settlements made in the range, each with its lines.
func (c *cashfree) ListSettlements(ctx context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	filters := map[string]any{}
	if tr.From > 0 {
		filters["start_date"] = time.UnixMilli(tr.From).UTC().Format(time.RFC3339)
	}
	if tr.To > 0 {
		filters["end_date"] = time.UnixMilli(tr.To).UTC().Format(time.RFC3339)
	}
	var out []connector.SettlementSnapshot
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("cashfree: more than %d pages of settlements", maxPages)
		}
		pg := map[string]any{"limit": settlementPage}
		if cursor != "" {
			pg["cursor"] = cursor
		}
		var res pageOf[settlementWire]
		if err := c.api.do(ctx, "POST", "/settlements", map[string]any{"pagination": pg, "filters": filters}, &res, ""); err != nil {
			return nil, err
		}
		for _, s := range res.Data {
			snap, err := c.settlement(ctx, s)
			if err != nil {
				return nil, err
			}
			out = append(out, snap)
		}
		if res.Cursor == "" || res.Cursor == cursor || len(res.Data) == 0 {
			return out, nil
		}
		cursor = res.Cursor
	}
}

func (c *cashfree) settlement(ctx context.Context, s settlementWire) (connector.SettlementSnapshot, error) {
	id := string(s.CFSettlementID)
	if id == "" {
		return connector.SettlementSnapshot{}, errors.New("cashfree: settlement without cf_settlement_id")
	}
	amt, _ := s.Amount.minor("INR")
	fees, _ := s.ChargesTotal.minor("INR")
	tax, _ := s.TaxTotal.minor("INR")
	snap := connector.SettlementSnapshot{ProviderSettlementID: id, Amount: amt, Fees: fees, Tax: tax, Currency: "INR",
		Status: s.Status, UTR: s.UTR, SettledAt: parseTime(s.Date)}
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxPages {
			return snap, fmt.Errorf("cashfree: more than %d pages of lines in settlement %s", maxPages, id)
		}
		pg := map[string]any{"limit": settlementPage}
		if cursor != "" {
			pg["cursor"] = cursor
		}
		var res pageOf[reconWire]
		in := map[string]any{"pagination": pg, "filters": map[string]any{"cf_settlement_ids": []string{id}}}
		if err := c.api.do(ctx, "POST", "/settlement/recon", in, &res, ""); err != nil {
			return snap, err
		}
		for _, r := range res.Data {
			a, _ := r.Amount.minor("INR")
			f, _ := r.Charge.minor("INR")
			t, _ := r.Tax.minor("INR")
			it := connector.SettlementItem{Amount: a, Fee: f, Tax: t}
			switch r.EventType {
			case "PAYMENT":
				it.Kind, it.ProviderPaymentID = "payment", paymentID(r.OrderID, r.CFPayment)
			case "REFUND":
				it.Kind, it.ProviderRefundID = "refund", r.RefundID
				if it.Amount > 0 {
					it.Amount = -it.Amount
				}
			default:
				it.Kind = "adjustment"
			}
			snap.Items = append(snap.Items, it)
		}
		if res.Cursor == "" || res.Cursor == cursor || len(res.Data) == 0 {
			return snap, nil
		}
		cursor = res.Cursor
	}
}

var _ connector.Settler = (*cashfree)(nil)
