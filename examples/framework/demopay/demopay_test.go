package demopay_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/paymux/paymux/examples/framework/demopay"
	"github.com/paymux/paymux/pkg/connector"
)

func TestDemoConnectorRegistersAndRedirects(t *testing.T) {
	factory, info, ok := connector.Lookup("demopay")
	if !ok || info.DisplayName == "" {
		t.Fatal("not registered by import")
	}
	if _, err := factory(json.RawMessage(`{}`), connector.Deps{}); err == nil {
		t.Error("a config without checkout_url must be refused")
	}
	c, err := factory(json.RawMessage(`{"checkout_url":"https://pay.example.com/c"}`), connector.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: "ref1", Amount: 100, Currency: "INR"})
	if err != nil || !res.NextAction.Valid() || !strings.HasPrefix(res.NextAction.URL, "https://pay.example.com/c") {
		t.Fatalf("checkout: %+v %v", res, err)
	}
}
