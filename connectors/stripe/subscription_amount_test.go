package stripe

import (
	"errors"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// subWithItem is a subscription whose single item carries the inline price.
func subWithItem() map[string]any {
	s := subJSON("active")
	s["items"] = map[string]any{"data": []any{map[string]any{"id": "si_1N1", "price": map[string]any{
		"unit_amount": 49900, "currency": "inr", "product": "prod_1N1",
		"recurring": map[string]any{"interval": "month", "interval_count": 3}}}}}
	return s
}

// A new amount is a new inline price on the subscription item, effective from
// the next invoice: no proration (Paymux decides that itself).
func TestUpdateSubscriptionAmountSwapsTheItemPriceWithoutProration(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/subscriptions/sub_1N1", 200, subWithItem())
	f.reply("POST /v1/subscriptions/sub_1N1", 200, subWithItem())
	c := newConn(t, f)
	if err := c.UpdateSubscriptionAmount(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_1N1"}, 59900); err != nil {
		t.Fatal(err)
	}
	posts := f.to("POST /v1/subscriptions/sub_1N1")
	if len(posts) != 1 {
		t.Fatalf("%d updates", len(posts))
	}
	for k, v := range map[string]string{
		"items[0][id]":                                    "si_1N1",
		"items[0][price_data][currency]":                  "inr",
		"items[0][price_data][product]":                   "prod_1N1",
		"items[0][price_data][unit_amount]":               "59900",
		"items[0][price_data][recurring][interval]":       "month",
		"items[0][price_data][recurring][interval_count]": "3",
		"proration_behavior":                              "none",
	} {
		if got := posts[0].Form.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if posts[0].Header.Get("Idempotency-Key") != "subamount:sub_1N1:59900" {
		t.Errorf("idempotency key = %q", posts[0].Header.Get("Idempotency-Key"))
	}
}

func TestUpdateSubscriptionAmountRefusesWhatItCannotDo(t *testing.T) {
	f := newFake()
	noItems := subJSON("active")
	noItems["items"] = map[string]any{"data": []any{}}
	f.reply("GET /v1/subscriptions/sub_none", 200, noItems)
	f.reply("GET /v1/subscriptions/sub_1N1", 200, subWithItem())
	c := newConn(t, f)

	before := f.count()
	if err := c.UpdateSubscriptionAmount(bg, connector.SubscriptionRef{ProviderSubscriptionID: "cs_test_s1"}, 100); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("before purchase: %v", err)
	}
	if err := c.UpdateSubscriptionAmount(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_1N1"}, 0); err == nil {
		t.Error("a zero amount was accepted")
	}
	if f.count() != before {
		t.Error("a request was made for an id that is not a subscription, or for a bad amount")
	}
	if err := c.UpdateSubscriptionAmount(bg, connector.SubscriptionRef{ProviderSubscriptionID: "sub_none"}, 100); err == nil {
		t.Error("a subscription without an item was updated")
	}
	if len(f.to("POST /v1/subscriptions/sub_none")) != 0 {
		t.Error("an update was sent for a subscription without an item")
	}
}
