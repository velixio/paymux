package invoice_test

import (
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// settle records a settlement of one attempt with the given fees, as
// settlement reconciliation does.
func (e *env) settle(account, providerID string, settledAt time.Time, a store.Attempt, amount, fee, feeTax, settlementFees, settlementTax int64, mismatch bool) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		row, err := store.UpsertSettlement(ctx, tx, store.Settlement{ID: "stl_" + providerID, OrgID: store.DefaultOrgID, Mode: "live", ProviderAccountID: account,
			ProviderSettlementID: providerID, Amount: amount - fee - feeTax, Fees: settlementFees, FeeTax: settlementTax, Currency: "INR", Status: "processed",
			UTR: "UTR" + providerID, SettledAt: settledAt}, settledAt)
		if err != nil {
			return err
		}
		if _, err := store.AttachSettlementFees(ctx, tx, a.ID, row.ID, fee, feeTax, amount-fee-feeTax); err != nil {
			return err
		}
		return store.SetSettlementFeeCheck(ctx, tx, row.ID, fee, feeTax, mismatch)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setProviderGSTIN(account, gstin string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		pa, err := store.GetProviderAccount(ctx, tx, account)
		if err != nil {
			return err
		}
		pa.ProviderGSTIN = gstin
		return store.UpdateProviderAccount(ctx, tx, pa, e.clock())
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestSettlementAndITCReports(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setNow(day(3))
		_, a1 := e.pay(118000, "INR", intraB2C, "pa_1")
		_, a2 := e.pay(59000, "INR", intraB2C, "pa_1")
		_, a3 := e.pay(59000, "INR", intraB2C, "pa_mor")
		e.setProviderGSTIN("pa_1", "29AAGCB7383J1Z4") // Karnataka: inter-state for a Maharashtra supplier
		e.setProviderGSTIN("pa_mor", "")
		e.settle("pa_1", "setl_A", day(20), a1, 118000, 2000, 360, 2000, 360, false)
		e.settle("pa_mor", "setl_B", day(22), a3, 59000, 1000, 181, 1000, 181, false) // odd GST paise, provider without a GSTIN
		e.settle("pa_1", "setl_C", day(25), a2, 59000, 900, 162, 1000, 162, true)     // fee totals differ: flagged
		e.settle("pa_1", "setl_Nov", time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), a2, 1, 0, 0, 0, 0, false)

		set, err := e.inv.Reports(ctx, "te_1", "2026-10")
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		var order []string
		for _, f := range set.Files {
			files[f.Name] = string(f.CSV)
			order = append(order, f.Name)
		}
		if n := len(order); n < 2 || order[n-2] != "settlements.csv" || order[n-1] != "itc_fees.csv" {
			t.Fatalf("the new sheets are appended after the existing ones: %v", order)
		}
		wantSettlements := strings.Join([]string{
			"settlement_id,provider,account,utr,status,settled_date,currency,amount,fees,fee_gst,payments,payment_fees,payment_fee_gst,fee_mismatch",
			"setl_A,fake,Fake,UTRsetl_A,processed,2026-10-20,INR,1156.40,20.00,3.60,1,20.00,3.60,no",
			"setl_B,mor,MoR,UTRsetl_B,processed,2026-10-22,INR,578.19,10.00,1.81,1,10.00,1.81,no",
			"setl_C,fake,Fake,UTRsetl_C,processed,2026-10-25,INR,579.38,10.00,1.62,1,9.00,1.62,yes",
			"",
		}, "\n")
		if files["settlements.csv"] != wantSettlements {
			t.Errorf("settlements.csv\n--- got ---\n%s\n--- want ---\n%s", files["settlements.csv"], wantSettlements)
		}
		wantITC := strings.Join([]string{
			"provider_gstin,provider,account,document_ref,date,currency,taxable_fee,igst,cgst,sgst,total_gst,itc_status",
			"29AAGCB7383J1Z4,fake,Fake,setl_A (UTRsetl_A),2026-10-20,INR,20.00,3.60,0.00,0.00,3.60,claimable",
			",mor,MoR,setl_B (UTRsetl_B),2026-10-22,INR,10.00,0.00,0.00,0.00,1.81,missing_provider_gstin",
			"29AAGCB7383J1Z4,fake,Fake,setl_C (UTRsetl_C),2026-10-25,INR,9.00,1.62,0.00,0.00,1.62,claimable",
			"",
		}, "\n")
		if files["itc_fees.csv"] != wantITC {
			t.Errorf("itc_fees.csv\n--- got ---\n%s\n--- want ---\n%s", files["itc_fees.csv"], wantITC)
		}
	})
}

func TestITCRegisterSplitsGSTWhenProviderIsInTheSuppliersState(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setNow(day(3))
		_, a := e.pay(118000, "INR", intraB2C, "pa_1")
		e.setProviderGSTIN("pa_1", supplierGSTIN) // same state as the supplier (Maharashtra): CGST + SGST
		e.settle("pa_1", "setl_A", day(20), a, 118000, 2000, 361, 2000, 361, false)
		set, err := e.inv.Reports(ctx, "te_1", "2026-10")
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for _, f := range set.Files {
			if f.Name == "itc_fees.csv" {
				got = string(f.CSV)
			}
		}
		want := supplierGSTIN + ",fake,Fake,setl_A (UTRsetl_A),2026-10-20,INR,20.00,0.00,1.80,1.81,3.61,claimable"
		if !strings.Contains(got, want) {
			t.Errorf("itc_fees.csv\n%s\nwant the row\n%s", got, want)
		}
	})
}

func TestSettlementReportsAreHeaderOnlyWithoutSettlements(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		set, err := e.inv.Reports(ctx, "te_1", "2026-10")
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range set.Files {
			if (f.Name == "settlements.csv" || f.Name == "itc_fees.csv") && strings.Count(string(f.CSV), "\n") != 1 {
				t.Errorf("%s: %q", f.Name, f.CSV)
			}
		}
	})
}
