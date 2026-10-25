package billingform_test

import (
	"net/url"
	"testing"

	"github.com/paymux/paymux/internal/billingform"
	"github.com/paymux/paymux/internal/store"
)

const gstin29 = "29AAAAA0000A1ZY"

func form(kv ...string) url.Values {
	f := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		f.Set(kv[i], kv[i+1])
	}
	return f
}

func TestNeedForIndianAndForeignBuyers(t *testing.T) {
	cust := func(country, state string) store.Customer {
		return store.Customer{Name: "A", Billing: store.Billing{Country: country, StateCode: state}}
	}
	for name, tc := range map[string]struct {
		mode, currency string
		issue          bool
		amount         int64
		c              store.Customer
		hasCustomer    bool
		want           billingform.Need
	}{
		"india, state needed":        {"state_required", "INR", true, 50000, cust("", ""), true, billingform.Need{Show: true, StateRequired: true}},
		"india, state known":         {"state_required", "INR", true, 50000, cust("IN", "29"), true, billingform.Need{StateRequired: true}},
		"india, large asks address":  {"state_required", "INR", true, 5_000_000, cust("IN", "29"), true, billingform.Need{Show: true, StateRequired: true, AddressRequired: true}},
		"india, invoicing off":       {"state_required", "INR", false, 50000, cust("", ""), true, billingform.Need{}},
		"abroad paying in rupees":    {"state_required", "INR", true, 50000, cust("US", ""), true, billingform.Need{}},
		"dollars, country unknown":   {"state_required", "USD", true, 1500, cust("", ""), true, billingform.Need{Show: true, CountryRequired: true}},
		"dollars, country known":     {"state_required", "USD", true, 1500, cust("US", ""), true, billingform.Need{}},
		"dollars, an Indian buyer":   {"state_required", "USD", true, 1500, cust("IN", ""), true, billingform.Need{Show: true, StateRequired: true}},
		"dollars, setting off":       {"off", "USD", true, 1500, cust("", ""), true, billingform.Need{}},
		"dollars, invoicing off":     {"state_required", "USD", false, 1500, cust("", ""), true, billingform.Need{}},
		"dollars, optional mode":     {"optional", "USD", true, 1500, cust("", ""), true, billingform.Need{Show: true, CountryRequired: true}},
		"rupees, optional, no buyer": {"optional", "INR", true, 50000, store.Customer{}, false, billingform.Need{Show: true}},
	} {
		got := billingform.NeedFor(tc.mode, tc.issue, tc.currency, tc.amount, tc.c, tc.hasCustomer)
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestParse(t *testing.T) {
	india := billingform.Need{Show: true, StateRequired: true}
	for name, tc := range map[string]struct {
		f        url.Values
		currency string
		need     billingform.Need
		errKey   string
		check    func(t *testing.T, in store.Billing)
	}{
		"state":              {form("name", "Asha", "state_code", "29"), "INR", india, "", func(t *testing.T, b store.Billing) { must(t, b.Country == "IN" && b.StateCode == "29", b) }},
		"no state":           {form("name", "Asha"), "INR", india, "billing.err.state_required", nil},
		"no name":            {form("state_code", "29"), "INR", india, "billing.err.name", nil},
		"bad state":          {form("name", "A", "state_code", "99"), "INR", india, "billing.err.state_invalid", nil},
		"gstin gives state":  {form("name", "A", "gstin", gstin29, "legal_name", "Acme"), "INR", india, "", func(t *testing.T, b store.Billing) { must(t, b.StateCode == "29" && b.GSTIN == gstin29, b) }},
		"gstin needs a name": {form("name", "A", "gstin", gstin29), "INR", india, "billing.err.legal_name", nil},
		"gstin checksum":     {form("name", "A", "gstin", "29AAAAA0000A1Z5", "legal_name", "X", "state_code", "29"), "INR", india, "billing.err.gstin", nil},
		"gstin vs state":     {form("name", "A", "gstin", gstin29, "legal_name", "X", "state_code", "27"), "INR", india, "billing.err.gst_state", nil},
		"abroad": {form("name", "A", "country", "us", "state_code", "29", "gstin", gstin29), "INR", india, "", func(t *testing.T, b store.Billing) {
			must(t, b.Country == "US" && b.StateCode == "" && b.GSTIN == "", b)
		}},
		"bad country":            {form("name", "A", "country", "USA"), "INR", india, "billing.err.country", nil},
		"dollars need a country": {form("name", "A"), "USD", billingform.Need{Show: true, CountryRequired: true}, "billing.err.country_required", nil},
		"dollars, a country":     {form("name", "A", "country", "DE"), "USD", billingform.Need{Show: true, CountryRequired: true}, "", func(t *testing.T, b store.Billing) { must(t, b.Country == "DE", b) }},
		"dollars, India":         {form("name", "A", "country", "IN", "state_code", "29"), "USD", billingform.Need{Show: true, CountryRequired: true}, "", func(t *testing.T, b store.Billing) { must(t, b.Country == "IN" && b.StateCode == "29", b) }},
		"address":                {form("name", "A", "state_code", "29"), "INR", billingform.Need{Show: true, StateRequired: true, AddressRequired: true}, "billing.err.address", nil},
		"pin":                    {form("name", "A", "state_code", "29", "line1", "1 MG Road", "postal_code", "12"), "INR", billingform.Need{Show: true, StateRequired: true, AddressRequired: true}, "billing.err.pin", nil},
	} {
		in, key := billingform.Parse(tc.f, tc.currency, tc.need)
		if key != tc.errKey {
			t.Errorf("%s: error %q, want %q", name, key, tc.errKey)
			continue
		}
		if tc.check != nil {
			tc.check(t, in.Billing)
		}
	}
}

func must(t *testing.T, ok bool, b store.Billing) {
	t.Helper()
	if !ok {
		t.Errorf("billing = %+v", b)
	}
}
