// Package recon compares Paymux records with provider listings (§11) and
// classifies differences. It is pure; the job layer fetches data, applies
// auto-fixes through the core service and stores ReconItems.
package recon

// Item kinds.
const (
	KindStatusMismatch        = "status_mismatch"
	KindAmountMismatch        = "amount_mismatch"
	KindCurrencyMismatch      = "currency_mismatch"
	KindUnknownProviderObject = "unknown_provider_object"
	KindRenewalToLink         = "renewal_to_link"
	KindFeeMismatch           = "fee_mismatch"
	KindExtraCapture          = "extra_capture"
)

// Record is a payment or refund as seen by one side, keyed by provider id.
type Record struct {
	ProviderID        string
	Status            string
	Amount            int64
	Currency          string
	SubscriptionID    string // provider subscription id, for provider-managed renewals
	CapturedElsewhere bool   // this attempt already booked a different provider payment id
}

// Item is one reconciliation finding.
type Item struct {
	Kind       string
	ProviderID string
	Ours       *Record
	Theirs     Record
	AutoFix    bool // provider state wins; apply via core transition
}

// Diff compares our records with the provider listing for the same window.
// knownSubscription reports whether a provider subscription id belongs to us.
func Diff(ours, theirs []Record, knownSubscription func(string) bool) []Item {
	byID := make(map[string]Record, len(ours))
	for _, r := range ours {
		byID[r.ProviderID] = r
	}
	var items []Item
	for _, t := range theirs {
		o, ok := byID[t.ProviderID]
		if !ok {
			kind := KindUnknownProviderObject
			if t.SubscriptionID != "" && knownSubscription != nil && knownSubscription(t.SubscriptionID) {
				kind = KindRenewalToLink
			}
			items = append(items, Item{Kind: kind, ProviderID: t.ProviderID, Theirs: t})
			continue
		}
		oc := o
		switch {
		case o.CapturedElsewhere && t.Status == "captured":
			items = append(items, Item{Kind: KindExtraCapture, ProviderID: t.ProviderID, Ours: &oc, Theirs: t})
		case o.Currency != t.Currency:
			items = append(items, Item{Kind: KindCurrencyMismatch, ProviderID: t.ProviderID, Ours: &oc, Theirs: t})
		case o.Amount != t.Amount:
			items = append(items, Item{Kind: KindAmountMismatch, ProviderID: t.ProviderID, Ours: &oc, Theirs: t})
		case o.Status != t.Status:
			items = append(items, Item{Kind: KindStatusMismatch, ProviderID: t.ProviderID, Ours: &oc, Theirs: t, AutoFix: true})
		}
	}
	return items
}

// Settlement is a provider settlement's fee totals.
type Settlement struct {
	ID           string
	Fees, FeeTax int64
}

// CheckSettlementFees compares settlement fee totals with per-payment fees.
// ok is true when both totals match.
func CheckSettlementFees(s Settlement, paymentFees, paymentFeeTax []int64) (Item, bool) {
	var f, ft int64
	for _, v := range paymentFees {
		f += v
	}
	for _, v := range paymentFeeTax {
		ft += v
	}
	if f == s.Fees && ft == s.FeeTax {
		return Item{}, true
	}
	return Item{Kind: KindFeeMismatch, ProviderID: s.ID}, false
}
