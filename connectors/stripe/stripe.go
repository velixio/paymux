// Package stripe is the Stripe connector. It talks to the REST API with a thin
// typed client (no provider SDK) and imports only the stdlib and pkg/....
//
// One-time payments use Checkout Sessions (mode payment) and a redirect;
// recurring billing is provider-managed Checkout Sessions in subscription mode
// with the price created inline. Payments are matched strictly by the session
// id this connector created (and the reference stored on it): nothing is ever
// found by searching.
//
// Verified against Stripe test mode (see the sandbox tests): Checkout Sessions
// in several currencies (retry with the same Idempotency-Key returns the same
// session), payment by a browser checkout and by API with test payment methods,
// strict matching by reference, unknown ids (404 resource_missing), bad keys
// (401), refunds (idempotent, over-refund 400), disputes (list, fetch, evidence
// with a real PNG, accept), subscriptions (fetch, repricing, cancel), balance
// transactions with fees, and the parsing of real events from /v1/events.
// Webhook payloads follow the API version of the webhook endpoint, not the
// version this connector pins: create the endpoint with API version 2024-06-20,
// or a renewal's invoice.paid will carry no payment intent (newer versions
// moved it off the invoice) and no renewal payment can be reported from it.
// Remaining TODO(spec) items are marked below.
package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["secret_key", "webhook_secret"],
  "additionalProperties": false,
  "properties": {
    "secret_key": {"type": "string", "title": "Secret key (sk_... or restricted rk_...)", "minLength": 8, "x-secret": true},
    "webhook_secret": {"type": "string", "title": "Webhook signing secret (whsec_...)", "minLength": 1, "x-secret": true}
  }
}`

var info = connector.Info{
	Version:      "0.1.0",
	DisplayName:  "Stripe",
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapList, connector.CapDisputeRead, connector.CapDisputeRespond, connector.CapSettle, connector.CapSubscriptionManager},
	ConfigSchema: json.RawMessage(configSchema),
	// A common subset; Stripe supports many more. An account narrows it.
	Currencies: []string{"USD", "EUR", "GBP", "INR", "AUD", "CAD", "SGD", "HKD", "JPY", "CHF", "SEK", "NOK", "DKK", "NZD", "AED", "BRL", "MXN", "PLN", "CZK", "KRW", "MYR", "THB"},
	// TODO(spec): the methods depend on the account's Checkout settings.
	Methods:           []string{"card", "wallet", "bank_transfer"},
	SubscriptionModes: []connector.BillingMode{connector.ProviderManaged},
	// Stripe's minimum charge amounts (https://docs.stripe.com/currencies#minimum-and-maximum-charge-amounts),
	// in the currency's minor units; a currency that is not here has no minimum
	// stated (one minor unit).
	MinChargeMinor: map[string]int64{"USD": 50, "EUR": 50, "GBP": 30, "INR": 50, "AUD": 50, "CAD": 50, "SGD": 50, "HKD": 400, "JPY": 50,
		"CHF": 50, "SEK": 300, "NOK": 300, "DKK": 250, "NZD": 50, "AED": 200, "BRL": 50, "MXN": 1000, "PLN": 200, "CZK": 1500, "MYR": 200, "THB": 1000},
	// Stripe allows 100 read and 100 write requests per second in live mode
	// and 25 in test mode; stay under the test limit.
	RateLimit: connector.RateLimit{RequestsPerSecond: 20, Burst: 20},
}

func init() { connector.Register("stripe", New, info) }

type config struct {
	SecretKey     string `json:"secret_key"`
	WebhookSecret string `json:"webhook_secret"`
}

type stripe struct {
	cfg  config
	deps connector.Deps
	api  *client
}

// New is the connector.Factory for Stripe.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("stripe: invalid config: %w", err)
	}
	if cfg.SecretKey == "" || cfg.WebhookSecret == "" {
		return nil, errors.New("stripe: secret_key and webhook_secret are required")
	}
	if !strings.HasPrefix(cfg.SecretKey, "sk_") && !strings.HasPrefix(cfg.SecretKey, "rk_") {
		return nil, errors.New("stripe: secret_key must be a secret (sk_...) or restricted (rk_...) key, not a publishable one")
	}
	hc := deps.HTTP
	if hc == nil {
		hc = connector.NewHTTPClient(connector.HTTPOptions{Logger: deps.Logger, Secrets: []string{cfg.SecretKey, cfg.WebhookSecret}, RateLimit: info.RateLimit})
	}
	return &stripe{cfg: cfg, deps: deps, api: &client{http: hc, key: cfg.SecretKey}}, nil
}

func (s *stripe) Info() connector.Info {
	i := info
	i.Name = "stripe"
	return i
}

// Metadata keys written on everything this connector creates.
const (
	metaRef     = "paymux_ref"
	metaPayment = "paymux_payment"
	metaAttempt = "paymux_attempt"
	metaSub     = "paymux_sub"
)

// withQuery appends a query parameter to a URL that may already have a query.
func withQuery(u, key, value string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + key + "=" + value
}

// CreateCheckout creates a Checkout Session (mode payment) and redirects the
// customer to its URL. The attempt's provider_ref is the session's
// client_reference_id and is stored in the metadata of the session and of its
// PaymentIntent, so the connector can later check that what it fetched is
// what it created.
func (s *stripe) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || len(req.ProviderRef) > 200 {
		return connector.CheckoutResult{}, errors.New("stripe: provider_ref must be 1..200 chars (it is the client_reference_id)")
	}
	amount, err := toStripe(req.Amount, req.Currency)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	if amount <= 0 {
		return connector.CheckoutResult{}, errors.New("stripe: amount must be positive")
	}
	ret := s.deps.URLs.ReturnURL(req.ProviderRef)
	f := url.Values{}
	f.Set("mode", "payment")
	f.Set("client_reference_id", req.ProviderRef)
	f.Set("success_url", withQuery(ret, "session_id", "{CHECKOUT_SESSION_ID}"))
	f.Set("cancel_url", withQuery(ret, "stripe_canceled", "1"))
	f.Set("line_items[0][quantity]", "1")
	f.Set("line_items[0][price_data][currency]", strings.ToLower(req.Currency))
	f.Set("line_items[0][price_data][unit_amount]", fmt.Sprint(amount))
	f.Set("line_items[0][price_data][product_data][name]", productName(req.Description))
	for k, v := range map[string]string{metaRef: req.ProviderRef, metaPayment: req.PaymentID, metaAttempt: req.AttemptID} {
		if v != "" {
			f.Set("metadata["+k+"]", v)
			f.Set("payment_intent_data[metadata]["+k+"]", v)
		}
	}
	if req.Customer.Email != "" {
		f.Set("customer_email", req.Customer.Email)
	}
	var sess session
	if err := s.api.do(ctx, "POST", "/v1/checkout/sessions", nil, f, &sess, "checkout:"+req.ProviderRef); err != nil {
		return connector.CheckoutResult{}, err
	}
	if sess.ID == "" || sess.URL == "" {
		return connector.CheckoutResult{}, errors.New("stripe: checkout session response lacks id or url")
	}
	if !strings.HasPrefix(sess.URL, "https://") {
		return connector.CheckoutResult{}, errors.New("stripe: checkout session url is not https")
	}
	return connector.CheckoutResult{ProviderOrderID: sess.ID, ExpiresAt: ms(sess.ExpiresAt),
		NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: sess.URL}}, nil
}

func productName(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "Payment"
	}
	if len(desc) > 250 {
		desc = desc[:250]
	}
	return desc
}

// HandleReturn checks the customer's return from Checkout. Stripe signs
// nothing on the redirect, so the session named in the URL is fetched and must
// carry this attempt's reference; a session id from another payment is
// ErrInvalidReturn. The caller must still fetch the payment itself.
func (s *stripe) HandleReturn(ctx context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	if req.Param("stripe_canceled") != "" {
		return connector.ReturnResult{ProviderRef: req.ProviderRef}, nil
	}
	sid := req.Param("session_id")
	if !strings.HasPrefix(sid, "cs_") || req.ProviderRef == "" {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	sess, err := s.getSession(ctx, sid)
	if errors.Is(err, connector.ErrNotFound) {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	if err != nil {
		return connector.ReturnResult{}, err
	}
	if sess.ClientReferenceID != req.ProviderRef {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: req.ProviderRef, ProviderOrderID: sess.ID, ProviderPaymentID: sess.PaymentIntent.ID}, nil
}

func ms(sec int64) int64 { return sec * 1000 }
