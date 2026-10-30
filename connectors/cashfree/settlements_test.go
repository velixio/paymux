package cashfree

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// Settlements are read from POST /settlement/recon, whose items nest the event,
// order, payment, refund and settlement details (testdata/settlement_recon.json
// follows Cashfree's published schema). Verified against the sandbox: with api
// version 2023-08-01 the date filters are start_date/end_date, a window may not
// exceed 30 days, cf_settlement_ids are integers, and some filter is required.
func TestListSettlementsFromRecon(t *testing.T) {
	f := newFake()
	f.on("POST /pg/settlement/recon", func(w http.ResponseWriter, c call) {
		var in struct {
			Filters map[string]any `json:"filters"`
		}
		_ = json.Unmarshal(c.Body, &in)
		if in.Filters["start_date"] == nil || in.Filters["end_date"] == nil {
			write(w, 400, map[string]any{"code": "request_failed", "message": "Atleast one of the filters needs to be present"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "settlement_recon"))
	})
	got, err := newConn(t, f).(connector.Settler).ListSettlements(context.Background(), connector.TimeRange{From: 1767225600000, To: 1767225600000 + 10*86400000})
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	s := got[0]
	if s.ProviderSettlementID != "1639775422" || s.UTR != "PGI45406" || s.Amount != 18764 || s.Fees != 200 || s.Tax != 36 || s.Currency != "INR" || s.SettledAt == 0 || len(s.Items) != 2 {
		t.Errorf("%+v", s)
	}
	pay, ref := s.Items[0], s.Items[1]
	if pay.Kind != "payment" || pay.ProviderPaymentID != "pay_ref_01:1234881617186388204" || pay.Amount != 20000 || pay.Fee != 200 || pay.Tax != 36 {
		t.Errorf("%+v", pay)
	}
	if ref.Kind != "refund" || ref.ProviderRefundID != "rf_ref_01" || ref.Amount != -1000 {
		t.Errorf("%+v", ref)
	}
	if adj := got[1]; adj.ProviderSettlementID != "1639775999" || len(adj.Items) != 1 || adj.Items[0].Kind != "adjustment" || adj.Items[0].Amount != 500 || adj.Amount != 500 {
		t.Errorf("%+v", adj)
	}
}

// A range longer than the 30 days the API allows is read in several windows;
// the settlements of the windows are merged by id.
func TestListSettlementsWindowsLongRanges(t *testing.T) {
	f := newFake()
	f.on("POST /pg/settlement/recon", func(w http.ResponseWriter, c call) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "settlement_recon"))
	})
	got, err := newConn(t, f).(connector.Settler).ListSettlements(context.Background(), connector.TimeRange{From: 1767225600000, To: 1767225600000 + 70*86400000})
	if err != nil || len(got) != 2 {
		t.Fatalf("settlements repeated in each window must merge: %d %v", len(got), err)
	}
	calls := f.to("POST /pg/settlement/recon")
	if len(calls) != 3 {
		t.Fatalf("70 days need 3 windows of at most 30: %d", len(calls))
	}
}

func TestListSettlementsWithoutARangeUsesTheLast30Days(t *testing.T) {
	f := newFake()
	f.reply("POST /pg/settlement/recon", 200, map[string]any{"cursor": nil, "data": []any{}, "limit": 0})
	if _, err := newConn(t, f).(connector.Settler).ListSettlements(context.Background(), connector.TimeRange{}); err != nil {
		t.Fatal(err)
	}
	in := f.to("POST /pg/settlement/recon")[0].json(t)["filters"].(map[string]any)
	if in["start_date"] == nil || in["end_date"] == nil {
		t.Errorf("a date range is required by the API: %v", in)
	}
}
