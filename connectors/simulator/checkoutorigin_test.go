package simulator

import (
	"net/http"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// urlsWithCheckout is the hub's URLs with PAYMUX_CHECKOUT_URL set: the /sim
// page lives on the checkout origin, the webhook on the base one.
type urlsWithCheckout struct{ connectortest.URLs }

func (urlsWithCheckout) CheckoutOrigin() string { return "https://checkout.example.test" }

// With a checkout origin the simulator's page is on that host, so the hosts it
// declares must name that host too, or the hub refuses its own simulator.
func TestCheckoutOnTheCheckoutOriginIsOnTheDeclaredHosts(t *testing.T) {
	deps := connector.Deps{HTTP: http.DefaultClient, KV: connector.NewMemKV(time.Now),
		URLs: urlsWithCheckout{connectortest.URLs{Base: "https://pay.example.test"}}}
	c, err := New([]byte(`{}`), deps)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CreateCheckout(ctx, connector.CheckoutRequest{ProviderRef: "ref_co", Amount: 100, Currency: "INR", PaymentID: "pay_x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := res.NextAction.Allowed(connector.HostsOf(c)); err != nil {
		t.Fatalf("the simulator's own checkout %q is refused: %v", res.NextAction.URL, err)
	}
}
