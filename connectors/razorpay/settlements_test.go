package razorpay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

var tIST = time.FixedZone("IST", 5*3600+1800)

func fakeSettlement(id string, amount, fees, tax int64, status string, at time.Time) map[string]any {
	return map[string]any{"id": id, "entity": "settlement", "amount": amount, "status": status, "fees": fees, "tax": tax,
		"utr": "UTR" + id, "created_at": at.Unix()}
}

func fakeReconLine(settlementID, typ, entityID string, amount, fee, tax int64) map[string]any {
	return map[string]any{"entity": "settlement", "entity_id": entityID, "type": typ, "amount": amount, "fee": fee, "tax": tax,
		"settlement_id": settlementID, "currency": "INR", "settled_at": 1760000000}
}

func TestListSettlements(t *testing.T) {
	day1 := time.Date(2026, 10, 15, 10, 0, 0, 0, tIST)
	day2 := time.Date(2026, 10, 16, 18, 30, 0, 0, tIST)
	f := newFake("captured")
	f.settlements = []map[string]any{
		fakeSettlement("setl_A", 99000, 1178, 180, "processed", day1),
		fakeSettlement("setl_B", 49000, 590, 90, "created", day2),
		fakeSettlement("setl_old", 1, 0, 0, "processed", day1.AddDate(0, -2, 0)),
	}
	f.recon = map[string][]any{
		"2026-10-15": {fakeReconLine("setl_A", "payment", "pay_1", 50000, 590, 90), fakeReconLine("setl_A", "payment", "pay_2", 50000, 588, 90),
			fakeReconLine("setl_A", "refund", "rfnd_1", -1000, 0, 0), fakeReconLine("setl_A", "adjustment", "adj_1", 0, 0, 0)},
		"2026-10-16": {fakeReconLine("setl_B", "payment", "pay_3", 49500, 590, 90)},
	}
	c := newConn(t, f)
	st, ok := c.(connector.Settler)
	if !ok {
		t.Fatal("razorpay must be a Settler")
	}
	if !c.Info().Has(connector.CapSettle) {
		t.Error("CapSettle not declared")
	}
	from, to := day1.Add(-time.Hour).UnixMilli(), day2.Add(time.Hour).UnixMilli()
	got, err := st.ListSettlements(context.Background(), connector.TimeRange{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("settlements %+v", got)
	}
	a, b := got[0], got[1]
	if a.ProviderSettlementID != "setl_A" || a.Amount != 99000 || a.Fees != 1178 || a.Tax != 180 || a.Currency != "INR" || a.Status != "processed" ||
		a.UTR != "UTRsetl_A" || a.SettledAt != day1.UnixMilli() {
		t.Errorf("A = %+v", a)
	}
	if b.ProviderSettlementID != "setl_B" || b.Status != "created" || len(b.Items) != 1 || b.Items[0].ProviderPaymentID != "pay_3" {
		t.Errorf("B = %+v", b)
	}
	want := []connector.SettlementItem{
		{Kind: "payment", ProviderPaymentID: "pay_1", Amount: 50000, Fee: 590, Tax: 90},
		{Kind: "payment", ProviderPaymentID: "pay_2", Amount: 50000, Fee: 588, Tax: 90},
		{Kind: "refund", ProviderRefundID: "rfnd_1", Amount: -1000},
		{Kind: "adjustment", Amount: 0},
	}
	if len(a.Items) != len(want) {
		t.Fatalf("items %+v", a.Items)
	}
	for i := range want {
		if a.Items[i] != want[i] {
			t.Errorf("item %d = %+v want %+v", i, a.Items[i], want[i])
		}
	}
	// The range is sent to Razorpay in unix seconds, "to" inclusive.
	calls := f.callsTo("GET", "/v1/settlements")
	if len(calls) == 0 || !strings.Contains(calls[0].Query, "from=") || !strings.Contains(calls[0].Query, "to=") {
		t.Errorf("calls %+v", calls)
	}
}

func TestListSettlementsPagesAndErrors(t *testing.T) {
	f := newFake("captured")
	at := time.Date(2026, 10, 15, 10, 0, 0, 0, tIST)
	for i := 0; i < 120; i++ { // more than one page of 100
		f.settlements = append(f.settlements, fakeSettlement("setl_"+string(rune('a'+i%26))+time.Duration(i).String(), 1, 0, 0, "processed", at))
	}
	c := newConn(t, f)
	got, err := c.(connector.Settler).ListSettlements(context.Background(), connector.TimeRange{From: at.Add(-time.Hour).UnixMilli(), To: at.Add(time.Hour).UnixMilli()})
	if err != nil || len(got) != 120 {
		t.Fatalf("paged settlements: %d %v", len(got), err)
	}
	// An unreachable API is a temporary error, bad credentials are reported as such.
	srv := startFake(t, f)
	bad, err := New([]byte(`{"key_id":"wrong","key_secret":"wrong","webhook_secret":"x"}`), connectortest.NewDeps(srv, time.Unix(1760000100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.(connector.Settler).ListSettlements(context.Background(), connector.TimeRange{From: 1, To: 2}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("err = %v", err)
	}
}
