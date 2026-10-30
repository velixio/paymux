package connectors_test

import (
	"slices"
	"testing"

	_ "github.com/velixio/paymux/connectors/cashfree"
	_ "github.com/velixio/paymux/connectors/payu"
	_ "github.com/velixio/paymux/connectors/razorpay"
	_ "github.com/velixio/paymux/connectors/stripe"
	_ "github.com/velixio/paymux/connectors/zoho"
	"github.com/velixio/paymux/pkg/connector"
)

// Info.MinChargeMinor states each provider's smallest charge, from its own
// documentation, in minor units. Paymux never asks for less.
func TestBuiltInConnectorsStateTheirMinimumCharge(t *testing.T) {
	want := map[string]map[string]int64{
		"razorpay": {"INR": 100},
		"payu":     {"INR": 100},
		"zoho":     {"INR": 100},
		"cashfree": {"INR": 100},
		"stripe": {"USD": 50, "EUR": 50, "GBP": 30, "INR": 50, "AUD": 50, "CAD": 50, "SGD": 50, "HKD": 400, "JPY": 50, "CHF": 50,
			"SEK": 300, "NOK": 300, "DKK": 250, "NZD": 50, "AED": 200, "BRL": 50, "MXN": 1000, "PLN": 200, "CZK": 1500, "MYR": 200, "THB": 1000},
	}
	for name, cur := range want {
		_, info, ok := connector.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		for c, v := range cur {
			if got := info.MinChargeMinor[c]; got != v {
				t.Errorf("%s %s: minimum %d, want %d", name, c, got, v)
			}
		}
	}
	// Whatever is declared is for a currency the connector takes, and positive.
	for _, info := range connector.List() {
		for c, v := range info.MinChargeMinor {
			if v <= 0 || !slices.Contains(info.Currencies, c) {
				t.Errorf("%s: MinChargeMinor[%q] = %d is not a positive minimum of one of its currencies", info.Name, c, v)
			}
		}
	}
}

func TestBuiltInConnectorsStateTheirMinimumRefund(t *testing.T) {
	_, info, ok := connector.Lookup("razorpay")
	if !ok || info.MinRefundMinor["INR"] != 100 {
		t.Fatalf("razorpay INR minimum refund = %d, want 100", info.MinRefundMinor["INR"])
	}
	for _, info := range connector.List() {
		for currency, minimum := range info.MinRefundMinor {
			if minimum <= 0 || !slices.Contains(info.Currencies, currency) {
				t.Errorf("%s: MinRefundMinor[%q] = %d is not a positive minimum of one of its currencies", info.Name, currency, minimum)
			}
		}
	}
}
