package connectors_test

import (
	"net/url"
	"testing"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/pkg/connector"
)

// B-7c: with a checkout URL the provider is sent back to the checkout host,
// while webhooks stay on the base host.
func TestReturnURLUsesTheCheckoutURL(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		base, _ := url.Parse("https://dash.example.com")
		checkout, _ := url.Parse("https://pay.example.com")
		e.svc = connectors.New(connectors.Options{DB: e.db, Box: e.box, BaseURL: base, CheckoutURL: checkout, Now: e.clk.Now})
		pa := e.create(fakeconn.ValidConfig)
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatal(err)
		}
		var urls connector.URLs
		e.fake.Locked(func() { urls = e.fake.Deps[len(e.fake.Deps)-1].URLs })
		if got := urls.ReturnURL("REF1"); got != "https://pay.example.com/return/"+pa.ID+"/REF1" {
			t.Errorf("return URL %q", got)
		}
		if got := urls.WebhookURL(); got != "https://dash.example.com/hooks/"+pa.ID {
			t.Errorf("webhook URL %q", got)
		}
		type checkoutOrigin interface{ CheckoutOrigin() string }
		if co, ok := urls.(checkoutOrigin); !ok || co.CheckoutOrigin() != "https://pay.example.com" {
			t.Errorf("checkout origin: %v", urls)
		}
	})
}
