package razorpay

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Razorpay reports settlement days in IST.
var ist = time.FixedZone("IST", 5*3600+1800)

const maxReconDays = 62

type settlementEntity struct {
	ID        string `json:"id"`
	Amount    int64  `json:"amount"`
	Status    string `json:"status"`
	Fees      int64  `json:"fees"`
	Tax       int64  `json:"tax"`
	UTR       string `json:"utr"`
	CreatedAt int64  `json:"created_at"`
}

// reconEntity is one line of the settlement reconciliation report.
type reconEntity struct {
	EntityID     string `json:"entity_id"`
	Type         string `json:"type"` // payment | refund | adjustment | ...
	Amount       int64  `json:"amount"`
	Debit        int64  `json:"debit"`
	Fee          int64  `json:"fee"`
	Tax          int64  `json:"tax"`
	SettlementID string `json:"settlement_id"`
}

// ListSettlements returns the settlements created in the range, each with its
// per-payment lines (fee and GST on the fee) taken from the settlement
// reconciliation report, "recon/combined", fetched for every IST day of the
// range.
//
// TODO(spec): wire details to re-verify against the live API: settled_at is
// taken from the settlement's created_at; a refund line's amount is read from
// amount, falling back to minus debit; lines of types other than payment and
// refund are reported as adjustments.
func (r *razorpay) ListSettlements(ctx context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	var setl []settlementEntity
	for skip := 0; ; {
		q := url.Values{"count": {strconv.Itoa(listPageSize)}, "skip": {strconv.Itoa(skip)}}
		if tr.From > 0 {
			q.Set("from", strconv.FormatInt(tr.From/1000, 10))
		}
		if tr.To > 0 {
			q.Set("to", strconv.FormatInt((tr.To-1)/1000+1, 10))
		}
		var c collection[settlementEntity]
		if err := r.api.do(ctx, "GET", "/v1/settlements", q, nil, &c, nil); err != nil {
			return nil, err
		}
		setl = append(setl, c.Items...)
		if len(c.Items) < listPageSize {
			break
		}
		skip += len(c.Items)
	}
	if len(setl) == 0 {
		return nil, nil
	}

	items := map[string][]connector.SettlementItem{}
	first := time.UnixMilli(tr.From).In(ist)
	last := time.UnixMilli(tr.To).In(ist)
	day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, ist)
	for n := 0; !day.After(last) && n < maxReconDays; n++ {
		for skip := 0; ; {
			q := url.Values{"year": {strconv.Itoa(day.Year())}, "month": {strconv.Itoa(int(day.Month()))}, "day": {strconv.Itoa(day.Day())},
				"count": {strconv.Itoa(listPageSize)}, "skip": {strconv.Itoa(skip)}}
			var c collection[reconEntity]
			if err := r.api.do(ctx, "GET", "/v1/settlements/recon/combined", q, nil, &c, nil); err != nil {
				return nil, fmt.Errorf("razorpay: settlement report for %s: %w", day.Format("2006-01-02"), err)
			}
			for _, e := range c.Items {
				if e.SettlementID != "" {
					items[e.SettlementID] = append(items[e.SettlementID], e.item())
				}
			}
			if len(c.Items) < listPageSize {
				break
			}
			skip += len(c.Items)
		}
		day = day.AddDate(0, 0, 1)
	}

	out := make([]connector.SettlementSnapshot, 0, len(setl))
	for _, s := range setl {
		out = append(out, connector.SettlementSnapshot{ProviderSettlementID: s.ID, Amount: s.Amount, Fees: s.Fees, Tax: s.Tax,
			Currency: "INR", Status: s.Status, UTR: s.UTR, SettledAt: ms(s.CreatedAt), Items: items[s.ID]})
	}
	return out, nil
}

func (e reconEntity) item() connector.SettlementItem {
	it := connector.SettlementItem{Amount: e.Amount, Fee: e.Fee, Tax: e.Tax}
	if it.Amount == 0 && e.Debit > 0 {
		it.Amount = -e.Debit
	}
	switch e.Type {
	case "payment":
		it.Kind, it.ProviderPaymentID = "payment", e.EntityID
	case "refund":
		it.Kind, it.ProviderRefundID = "refund", e.EntityID
	default:
		it.Kind = "adjustment"
	}
	return it
}

var _ connector.Settler = (*razorpay)(nil)
