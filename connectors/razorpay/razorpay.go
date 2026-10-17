// Package razorpay is the Razorpay connector. It talks to the REST API with a
// thin typed client (no provider SDK) and imports only the stdlib and pkg/....
package razorpay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/paymux/paymux/pkg/connector"
)

const (
	checkoutScript = "https://checkout.razorpay.com/v1/checkout.js"
	maxReceiptLen  = 40
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["key_id", "key_secret", "webhook_secret"],
  "additionalProperties": false,
  "properties": {
    "key_id": {"type": "string", "title": "Key ID", "minLength": 1},
    "key_secret": {"type": "string", "title": "Key secret", "minLength": 1, "x-secret": true},
    "webhook_secret": {"type": "string", "title": "Webhook secret", "minLength": 1, "x-secret": true},
    "name": {"type": "string", "title": "Checkout name (optional)"},
    "logo": {"type": "string", "title": "Checkout logo URL (optional)", "format": "uri"}
  }
}`

var info = connector.Info{
	Version:           "0.1.0",
	DisplayName:       "Razorpay",
	Capabilities:      []connector.Capability{connector.CapRefund, connector.CapList, connector.CapDisputeRead, connector.CapSubscriptionManager, connector.CapSettle},
	ConfigSchema:      json.RawMessage(configSchema),
	Currencies:        []string{"INR", "USD", "EUR", "GBP", "SGD", "AED", "AUD", "CAD"},
	Methods:           []string{"card", "upi", "netbanking", "wallet", "emi"},
	SubscriptionModes: []connector.BillingMode{connector.ProviderManaged},
	// TODO(spec): DisputeResponder (accept/contest) needs the multipart Documents
	// API; left out of Capabilities until it is built.
	// Razorpay does not publish a hard rate limit; stay well under typical
	// per-key throttling.
	RateLimit: connector.RateLimit{RequestsPerSecond: 20, Burst: 20},
}

func init() { connector.Register("razorpay", New, info) }

type config struct {
	KeyID         string `json:"key_id"`
	KeySecret     string `json:"key_secret"`
	WebhookSecret string `json:"webhook_secret"`
	Name          string `json:"name"`
	Logo          string `json:"logo"`
}

type razorpay struct {
	cfg  config
	deps connector.Deps
	api  *client
}

// New is the connector.Factory for Razorpay.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("razorpay: invalid config: %w", err)
	}
	if cfg.KeyID == "" || cfg.KeySecret == "" || cfg.WebhookSecret == "" {
		return nil, errors.New("razorpay: key_id, key_secret and webhook_secret are required")
	}
	hc := deps.HTTP
	if hc == nil {
		hc = connector.NewHTTPClient(connector.HTTPOptions{
			Logger:    deps.Logger,
			Secrets:   []string{cfg.KeySecret, cfg.WebhookSecret},
			RateLimit: info.RateLimit,
		})
	}
	return &razorpay{cfg: cfg, deps: deps, api: &client{http: hc, keyID: cfg.KeyID, keySecret: cfg.KeySecret}}, nil
}

func (r *razorpay) Info() connector.Info {
	i := info
	i.Name = "razorpay"
	return i
}

// CreateCheckout creates a Razorpay order and returns the checkout.js options.
func (r *razorpay) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || len(req.ProviderRef) > maxReceiptLen {
		return connector.CheckoutResult{}, fmt.Errorf("razorpay: provider_ref must be 1..%d chars (used as receipt)", maxReceiptLen)
	}
	if req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("razorpay: amount must be positive")
	}
	in := map[string]any{
		"amount":   req.Amount, // paise
		"currency": req.Currency,
		"receipt":  req.ProviderRef,
		"notes":    map[string]string{"pay": req.PaymentID, "att": req.AttemptID, "app": req.AppID},
	}
	var order struct {
		ID string `json:"id"`
	}
	if err := r.api.do(ctx, "POST", "/v1/orders", nil, in, &order, nil); err != nil {
		return connector.CheckoutResult{}, err
	}
	if order.ID == "" {
		return connector.CheckoutResult{}, errors.New("razorpay: order response has no id")
	}
	opts := map[string]any{
		"key":      r.cfg.KeyID,
		"order_id": order.ID,
		// float64 so the value is identical before and after a JSON round trip;
		// exact for any amount below 2^53.
		"amount":       float64(req.Amount),
		"currency":     req.Currency,
		"name":         r.cfg.Name,
		"callback_url": r.deps.URLs.ReturnURL(req.ProviderRef),
		// Razorpay only POSTs back to callback_url when redirect is true.
		"redirect": true,
		"prefill":  map[string]any{"name": req.Customer.Name, "email": req.Customer.Email, "contact": req.Customer.Phone},
	}
	if r.cfg.Logo != "" {
		opts["image"] = r.cfg.Logo
	}
	if req.Description != "" {
		opts["description"] = req.Description
	}
	return connector.CheckoutResult{
		ProviderOrderID: order.ID,
		NextAction:      connector.NextAction{Kind: connector.ActionSDK, ScriptURL: checkoutScript, Options: opts},
	}, nil
}

// HandleReturn verifies the checkout.js callback POST. Order payments sign
// HMAC-SHA256(key_secret, order_id|payment_id); subscription payments sign
// payment_id|subscription_id (the operands are in the other order). The caller
// must still fetch the truth.
func (r *razorpay) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	payID, sig := req.Param("razorpay_payment_id"), req.Param("razorpay_signature")
	orderID, subID := req.Param("razorpay_order_id"), req.Param("razorpay_subscription_id")
	var msg, parent string
	switch {
	case subID != "":
		msg, parent = payID+"|"+subID, subID
	case orderID != "":
		msg, parent = orderID+"|"+payID, orderID
	}
	if parent == "" || payID == "" || sig == "" || !validHMAC(r.cfg.KeySecret, []byte(msg), sig) {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: req.ProviderRef, ProviderOrderID: parent, ProviderPaymentID: payID}, nil
}

// validHMAC compares a hex HMAC-SHA256 signature in constant time.
func validHMAC(secret string, msg []byte, sigHex string) bool {
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(msg)
	return hmac.Equal(m.Sum(nil), got)
}
