package web_test

import (
	"strings"
	"testing"

	_ "github.com/paymux/paymux/connectors/cashfree"
	_ "github.com/paymux/paymux/connectors/payu"
	_ "github.com/paymux/paymux/connectors/razorpay"
	_ "github.com/paymux/paymux/connectors/simulator"
	_ "github.com/paymux/paymux/connectors/stripe"
	_ "github.com/paymux/paymux/connectors/whop"
	_ "github.com/paymux/paymux/connectors/zoho"
	"github.com/paymux/paymux/internal/web"
)

// What a connector declares per directive. The hosts come from the provider
// documentation and the endpoints in the connector's code; every test and live
// host is listed because the connector's Info does not depend on the mode.
func TestEveryConnectorDeclaresItsCheckoutHosts(t *testing.T) {
	cases := []struct {
		connector, directive string
		want                 []string
	}{
		{"razorpay", "script-src", []string{"https://checkout.razorpay.com"}},
		{"razorpay", "frame-src", []string{"https://api.razorpay.com", "https://checkout.razorpay.com"}},
		{"razorpay", "connect-src", []string{"https://api.razorpay.com", "https://lumberjack.razorpay.com"}},
		{"cashfree", "script-src", []string{"https://sdk.cashfree.com"}},
		{"cashfree", "frame-src", []string{"https://payments.cashfree.com", "https://sandbox.cashfree.com", "https://payments-test.cashfree.com"}},
		{"cashfree", "connect-src", []string{"https://api.cashfree.com", "https://sandbox.cashfree.com", "https://payments.cashfree.com"}},
		{"payu", "form-action", []string{"https://secure.payu.in", "https://test.payu.in"}},
		{"zoho", "script-src", []string{"https://static.zohocdn.com"}},
		{"zoho", "frame-src", []string{"https://*.zoho.com", "https://*.zoho.in"}},
		{"zoho", "connect-src", []string{"https://*.zoho.com", "https://*.zoho.in"}},
	}
	for _, c := range cases {
		got := directive(t, web.HostedCSP("n", c.connector, "", ""), c.directive)
		for _, h := range c.want {
			if !strings.Contains(" "+got+" ", " "+h+" ") {
				t.Errorf("%s: %s lacks %s (%s)", c.connector, c.directive, h, got)
			}
		}
	}
	// Redirecting connectors need no host: their page stays on the hub.
	for _, name := range []string{"stripe", "whop", "simulator"} {
		if p := web.HostedCSP("n", name, "", ""); strings.Contains(p, "https://") {
			t.Errorf("%s: unexpected host in %s", name, p)
		}
	}
}
