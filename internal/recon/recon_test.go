package recon

import (
	"reflect"
	"sort"
	"testing"
)

func kinds(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Kind+":"+it.ProviderID)
	}
	sort.Strings(out)
	return out
}

func TestDiffPayments(t *testing.T) {
	ours := []Record{
		{ProviderID: "pay_A", Status: "succeeded", Amount: 49900, Currency: "INR"},
		{ProviderID: "pay_B", Status: "pending", Amount: 10000, Currency: "INR"},
		{ProviderID: "pay_C", Status: "succeeded", Amount: 10000, Currency: "INR"},
		{ProviderID: "pay_D", Status: "succeeded", Amount: 500, Currency: "INR"},
		{ProviderID: "pay_SUB", Status: "succeeded", Amount: 1000, Currency: "INR"},
	}
	theirs := []Record{
		{ProviderID: "pay_A", Status: "succeeded", Amount: 49900, Currency: "INR"},           // match
		{ProviderID: "pay_B", Status: "succeeded", Amount: 10000, Currency: "INR"},           // status mismatch -> auto-fix
		{ProviderID: "pay_C", Status: "succeeded", Amount: 9900, Currency: "INR"},            // amount mismatch -> flag
		{ProviderID: "pay_D", Status: "succeeded", Amount: 500, Currency: "USD"},             // currency mismatch -> flag
		{ProviderID: "pay_X", Status: "succeeded", Amount: 700, Currency: "INR"},             // unknown
		{ProviderID: "pay_R", Status: "succeeded", Amount: 1000, Currency: "INR", SubscriptionID: "sub_rzp_1"}, // renewal of known sub
	}
	known := func(providerSubID string) bool { return providerSubID == "sub_rzp_1" }
	items := Diff(ours, theirs, known)
	want := []string{
		KindAmountMismatch + ":pay_C",
		KindCurrencyMismatch + ":pay_D",
		KindRenewalToLink + ":pay_R",
		KindStatusMismatch + ":pay_B",
		KindUnknownProviderObject + ":pay_X",
	}
	sort.Strings(want)
	if got := kinds(items); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for _, it := range items {
		switch it.Kind {
		case KindStatusMismatch:
			if !it.AutoFix || it.Theirs.Status != "succeeded" {
				t.Fatalf("status mismatch should auto-fix to provider state: %+v", it)
			}
		case KindAmountMismatch, KindCurrencyMismatch, KindUnknownProviderObject:
			if it.AutoFix {
				t.Fatalf("%s must be flag-only", it.Kind)
			}
		}
	}
}

func TestOursMissingAtProviderIsIgnoredInWindow(t *testing.T) {
	// Records we have but the provider listing lacks are not flagged (listing
	// windows and pending checkouts make that normal); the poller handles them.
	items := Diff([]Record{{ProviderID: "pay_Z", Status: "pending", Amount: 1, Currency: "INR"}}, nil, nil)
	if len(items) != 0 {
		t.Fatalf("got %v", items)
	}
}

func TestSettlementFeeMismatch(t *testing.T) {
	s := Settlement{ID: "setl_1", Fees: 1180, FeeTax: 180}
	if it, ok := CheckSettlementFees(s, []int64{500, 500}, []int64{90, 90}); ok || it.Kind != KindFeeMismatch {
		t.Fatalf("expected fee mismatch, got %+v %v", it, ok)
	}
	if _, ok := CheckSettlementFees(s, []int64{500, 500}, []int64{90, 90 + 0}); ok {
		t.Fatal("fees 1000 != 1180 should mismatch")
	}
	if _, ok := CheckSettlementFees(Settlement{ID: "s", Fees: 1000, FeeTax: 180}, []int64{500, 500}, []int64{90, 90}); !ok {
		t.Fatal("matching fees flagged")
	}
}
