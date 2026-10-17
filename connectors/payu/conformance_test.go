package payu

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

func TestConformance(t *testing.T) {
	cases := []struct {
		raw  string
		want connector.PaymentStatus
	}{
		{"success", connector.PaymentSucceeded},
		{"failure", connector.PaymentFailed},
		{"pending", connector.PaymentProcessing},
		{"userCancelled", connector.PaymentCanceled},
		{"Not Found", connector.PaymentRequiresAction},
	}
	var sc []connectortest.StatusCase
	for _, c := range cases {
		sc = append(sc, connectortest.StatusCase{Raw: c.raw, Want: c.want, WantAmount: 49900, Handler: http.Handler(&fake{verify: verifyBody(c.raw, "x")})})
	}
	ok := successForm(testSalt, "")
	connectortest.Run(t, New, connectortest.Fixtures{
		Config:   testConfig,
		Now:      time.Unix(1760000000, 0),
		Ref:      connector.PaymentRef{ProviderRef: "pay_ref_01"},
		Statuses: sc,
		Webhooks: connectortest.WebhookFixtures{
			Valid:       webhook(ok),
			WrongSecret: webhook(successForm("other-salt", "")),
			// PayU signs inside the body; there is no signature header to remove.
		},
		Return: &connectortest.ReturnFixtures{
			Valid:    connector.ReturnRequest{ProviderRef: "pay_ref_01", Form: ok},
			Tampered: connector.ReturnRequest{ProviderRef: "pay_ref_01", Form: tamper(ok)},
			Want:     connector.ReturnResult{ProviderRef: "pay_ref_01", ProviderPaymentID: "403993715521"},
		},
		Checkout: connector.CheckoutRequest{
			AttemptID: "att_01", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR", Description: "Pro plan",
			Customer: connector.Customer{Name: "Asha", Email: "asha@example.com"},
		},
	})
}

func tamper(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		out[k] = v
	}
	out.Set("amount", "1.00")
	return out
}
