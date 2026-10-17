package simulator

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

var ist = time.FixedZone("IST", 5*3600+1800)

// ListSettlements groups the captured payments of the range into one
// settlement per day (IST), with the simulator's flat fees: 2% of each
// payment plus 18% GST on that fee. Refunds that processed in the range are
// negative lines. The settlement is "processed" the day after.
func (c *conn) ListSettlements(ctx context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	keys, err := c.list(ctx, "list:pay")
	if err != nil {
		return nil, err
	}
	byDay := map[string]*connector.SettlementSnapshot{}
	day := func(ms int64) *connector.SettlementSnapshot {
		d := time.UnixMilli(ms).In(ist).Format("20060102")
		s, ok := byDay[d]
		if !ok {
			t, _ := time.ParseInLocation("20060102", d, ist)
			s = &connector.SettlementSnapshot{ProviderSettlementID: "sim_stl_" + d, Currency: "", Status: "processed",
				UTR: "SIMUTR" + d, SettledAt: t.Add(36 * time.Hour).UnixMilli()}
			byDay[d] = s
		}
		return s
	}
	for _, k := range keys {
		var p payRec
		if ok, err := c.load(ctx, "pay:"+k, &p); err != nil || !ok || p.Status != stCaptured {
			continue
		}
		if p.UpdatedAt < tr.From || tr.To != 0 && p.UpdatedAt >= tr.To {
			continue
		}
		fee, tax := p.fee()
		s := day(p.UpdatedAt)
		s.Currency = p.Currency
		s.Items = append(s.Items, connector.SettlementItem{Kind: "payment", ProviderPaymentID: p.PaymentID, Amount: p.Amount, Fee: fee, Tax: tax})
		s.Fees += fee
		s.Tax += tax
		s.Amount += p.Amount - fee - tax
	}
	refunds, err := c.list(ctx, "list:refund")
	if err != nil {
		return nil, err
	}
	for _, id := range refunds {
		var r refRec
		if ok, err := c.load(ctx, "ref:"+id, &r); err != nil || !ok || r.Status != "processed" {
			continue
		}
		if r.CreatedAt < tr.From || tr.To != 0 && r.CreatedAt >= tr.To {
			continue
		}
		s := day(r.CreatedAt)
		s.Currency = r.Currency
		s.Items = append(s.Items, connector.SettlementItem{Kind: "refund", ProviderPaymentID: r.PaymentID, ProviderRefundID: r.ID, Amount: -r.Amount})
		s.Amount -= r.Amount
	}
	ids := make([]string, 0, len(byDay))
	for d := range byDay {
		ids = append(ids, d)
	}
	sort.Strings(ids)
	out := make([]connector.SettlementSnapshot, 0, len(ids))
	for _, d := range ids {
		s := byDay[d]
		if s.Currency == "" {
			return nil, fmt.Errorf("simulator: settlement %s has no currency", s.ProviderSettlementID)
		}
		out = append(out, *s)
	}
	return out, nil
}
