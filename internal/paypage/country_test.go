package paypage_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
)

// The buyer's country decides how GST is charged (a buyer abroad is an
// export), so the pay page asks for it and keeps it on the customer.

func TestBillingFormAsksForTheCountryAndStoresIt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.inrPayment(50000)
		if r := e.do("GET", c.path(), "", nil); !strings.Contains(r.body, `name="country"`) {
			t.Fatalf("the billing form has no country field: %s", r.body)
		}
		// A buyer abroad paying in rupees: no Indian State needed, the country is kept.
		r := e.billingPost(c, func(f url.Values) { f.Del("state_code"); f.Set("country", "us") })
		if r.status != 303 {
			t.Fatalf("foreign buyer: %d %s", r.status, r.body)
		}
		cust, err := e.core.GetCustomer(ctx, e.payment(c.id).CustomerID)
		if err != nil || cust.Billing.Country != "US" || cust.Billing.StateCode != "" {
			t.Errorf("customer = %+v %v", cust.Billing, err)
		}
		// No country typed: an Indian buyer.
		c2 := e.inrPayment(50000)
		if r := e.billingPost(c2, nil); r.status != 303 {
			t.Fatalf("indian buyer: %d %s", r.status, r.body)
		}
		cust, _ = e.core.GetCustomer(ctx, e.payment(c2.id).CustomerID)
		if cust.Billing.Country != "IN" || cust.Billing.StateCode != "29" {
			t.Errorf("customer = %+v", cust.Billing)
		}
		// Not a country code.
		c3 := e.inrPayment(50000)
		for _, bad := range []string{"USA", "U1", "?!"} {
			if r := e.billingPost(c3, func(f url.Values) { f.Set("country", bad) }); r.status != 400 {
				t.Errorf("country %q: %d", bad, r.status)
			}
		}
	})
}

func TestBillingFormSkippedForAnApiSuppliedForeignBuyer(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{Currency: "INR", Amount: 50000,
			Customer: &core.CustomerInput{Name: "Bob", Billing: store.Billing{Country: "US"}}})
		if r := e.do("GET", c.path(), "", nil); r.status != 303 {
			t.Errorf("a foreign buyer needs no Indian State: %d %s", r.status, r.body)
		}
	})
}
