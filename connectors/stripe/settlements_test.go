package stripe

import (
	"net/http"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func bt(id, typ string, amount, fee int64, source map[string]any, tax int64) map[string]any {
	details := []any{map[string]any{"amount": fee - tax, "type": "stripe_fee"}}
	if tax > 0 {
		details = append(details, map[string]any{"amount": tax, "type": "tax"})
	}
	return map[string]any{"id": id, "type": typ, "amount": amount, "fee": fee, "currency": "inr", "source": source, "fee_details": details}
}

func TestSettlementsAreStripePayoutsWithTheirTransactions(t *testing.T) {
	f := newFake()
	f.on("GET /v1/payouts", func(w http.ResponseWriter, c call) {
		if c.Query.Get("starting_after") == "po_1" {
			write(w, 200, map[string]any{"data": []any{map[string]any{"id": "po_2", "amount": 1000, "currency": "inr", "status": "in_transit", "arrival_date": nowUnix + 86400}}, "has_more": false})
			return
		}
		write(w, 200, map[string]any{"has_more": true, "data": []any{map[string]any{"id": "po_1", "amount": 58882, "currency": "inr", "status": "paid",
			"arrival_date": nowUnix, "trace_id": map[string]any{"status": "supported", "value": "UTR123456"}}}})
	})
	f.on("GET /v1/balance_transactions", func(w http.ResponseWriter, c call) {
		switch c.Query.Get("payout") {
		case "po_1":
			if c.Query.Get("starting_after") == "txn_p" {
				write(w, 200, map[string]any{"has_more": false, "data": []any{bt("txn_3", "adjustment", -500, 0, map[string]any{"id": "adj_1", "object": "adjustment"}, 0)}})
				return
			}
			write(w, 200, map[string]any{"has_more": true, "data": []any{
				bt("txn_1", "charge", 49900, 1475, map[string]any{"id": "ch_3A1", "object": "charge", "payment_intent": "pi_3A1"}, 225), // fee 12.50 + GST 2.25
				bt("txn_2", "refund", -10000, 0, map[string]any{"id": "re_3A1", "object": "refund"}, 0),
				bt("txn_p", "payout", -39900, 0, map[string]any{"id": "po_1", "object": "payout"}, 0),
			}})
		default:
			write(w, 200, map[string]any{"has_more": false, "data": []any{}})
		}
	})
	c := newConn(t, f)
	out, err := c.ListSettlements(bg, connector.TimeRange{From: 1760000000000, To: 1760172800000})
	if err != nil || len(out) != 2 {
		t.Fatalf("settlements = %+v %v", out, err)
	}
	s := out[0]
	if s.ProviderSettlementID != "po_1" || s.Amount != 58882 || s.Currency != "INR" || s.Status != "paid" || s.UTR != "UTR123456" || s.SettledAt != nowUnix*1000 {
		t.Errorf("settlement = %+v", s)
	}
	want := []connector.SettlementItem{
		{Kind: "payment", ProviderPaymentID: "pi_3A1", Amount: 49900, Fee: 1250, Tax: 225},
		{Kind: "refund", ProviderRefundID: "re_3A1", Amount: -10000},
		{Kind: "adjustment", Amount: -500},
	}
	if len(s.Items) != len(want) {
		t.Fatalf("items = %+v", s.Items)
	}
	for i, w := range want {
		if s.Items[i] != w {
			t.Errorf("item %d = %+v, want %+v", i, s.Items[i], w)
		}
	}
	if s.Fees != 1250 || s.Tax != 225 {
		t.Errorf("totals fees=%d tax=%d (the payout line must not count)", s.Fees, s.Tax)
	}
	if out[1].ProviderSettlementID != "po_2" || out[1].Status != "in_transit" || len(out[1].Items) != 0 {
		t.Errorf("second = %+v", out[1])
	}
	q := f.to("GET /v1/payouts")[0].Query
	if q.Get("arrival_date[gte]") != "1760000000" || q.Get("arrival_date[lt]") != "1760172800" {
		t.Errorf("range = %v", q)
	}
	if e := f.to("GET /v1/balance_transactions")[0].Query["expand[]"]; len(e) != 1 || e[0] != "data.source" {
		t.Errorf("expand = %v", e)
	}
}
