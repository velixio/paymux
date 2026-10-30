package httpapi_test

import (
	"reflect"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// The routing restrictions a payment was created with come back on the
// payment, from create and from every read.
func TestPaymentShowsItsRoutingRestrictions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		r := f.do("POST", "/v1/payments", f.testKey, map[string]any{"amount": 100, "currency": "USD", "country": "US", "allowed_connectors": []string{"stripe", "razorpay"}})
		if r.status != 201 {
			t.Fatalf("%d %s", r.status, r.raw)
		}
		m := r.json(t)
		want := []any{"stripe", "razorpay"}
		if !reflect.DeepEqual(m["allowed_connectors"], want) || m["country"] != "US" {
			t.Errorf("create: %s", r.raw)
		}
		id := m["id"].(string)
		g := f.do("GET", "/v1/payments/"+id, f.testKey, nil).json(t)
		if !reflect.DeepEqual(g["allowed_connectors"], want) || g["country"] != "US" {
			t.Errorf("get: %v", g)
		}
		// Unrestricted payments say nothing about it.
		plain := f.do("POST", "/v1/payments", f.testKey, map[string]any{"amount": 100, "currency": "INR"}).json(t)
		if _, ok := plain["allowed_connectors"]; ok {
			t.Errorf("an unrestricted payment lists allowed_connectors: %v", plain)
		}
		if _, ok := plain["country"]; ok {
			t.Errorf("a payment without a country lists one: %v", plain)
		}
	})
}
