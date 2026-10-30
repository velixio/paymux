// Package demopay is the smallest useful connector: a stand-in for a payment
// provider that Paymux does not ship with. It sends the customer to a hosted
// page and never confirms a payment. Replace it with a real integration;
// docs/connectors.md describes the full contract and the conformance kit.
//
// It imports only pkg/connector and the standard library, like every
// connector.
package demopay

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/velixio/paymux/pkg/connector"
)

// Name is the connector's registry name.
const Name = "demopay"

const schema = `{"type":"object","required":["checkout_url"],"additionalProperties":false,
"properties":{"checkout_url":{"type":"string","minLength":1}}}`

func init() {
	connector.Register(Name, newConnector, info())
}

func info() connector.Info {
	return connector.Info{
		Name: Name, Version: "0.1.0", DisplayName: "Demo provider",
		ConfigSchema: json.RawMessage(schema), Currencies: []string{"INR", "USD"},
		RateLimit: connector.RateLimit{RequestsPerSecond: 10, Burst: 10},
	}
}

type demo struct{ checkoutURL string }

func newConnector(cfg json.RawMessage, _ connector.Deps) (connector.Connector, error) {
	var c struct {
		CheckoutURL string `json:"checkout_url"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil || c.CheckoutURL == "" {
		return nil, errors.New("demopay: checkout_url is required")
	}
	return &demo{checkoutURL: c.CheckoutURL}, nil
}

func (d *demo) Info() connector.Info { return info() }

func (d *demo) CreateCheckout(_ context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{
		ProviderOrderID: req.ProviderRef,
		NextAction:      connector.NextAction{Kind: connector.ActionRedirect, URL: d.checkoutURL + "?ref=" + req.ProviderRef},
	}, nil
}

func (d *demo) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{ProviderRef: req.ProviderRef}, nil
}

func (d *demo) VerifyWebhook(context.Context, connector.WebhookRequest) error {
	return connector.ErrInvalidSignature // this provider sends no webhooks
}

func (d *demo) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return nil, connector.ErrUnsupported
}

func (d *demo) FetchPayment(context.Context, connector.PaymentRef) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{}, connector.ErrNotFound
}
