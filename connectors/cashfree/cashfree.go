// Package cashfree is the Cashfree Payments (India) connector. It talks to the
// PG REST API (Orders, Payments, Refunds, Settlements) with a thin typed client
// (no provider SDK) and imports only the stdlib and pkg/....
//
// An order is created with order_id equal to the attempt's provider_ref, and
// everything is matched on that id: payments are fetched under the order and
// each must name the same order_id. Nothing is searched for.
//
// Cashfree's refunds belong to an order, but Paymux's refund request carries
// only the payment id; the connector therefore reports a payment's id as
// "<order_id>:<cf_payment_id>" so the order is always at hand.
//
// TODO(spec): written from Cashfree's documentation, not yet run against the
// real API (see sandbox_test.go). Real APIs differ from their docs: re-verify
// each TODO(spec) below at sandbox time.
package cashfree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

const (
	sdkScript    = "https://sdk.cashfree.com/js/v3/cashfree.js"
	maxOrderID   = 50
	maxRefundID  = 40
	fallbackMSDN = "9999999999"
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["client_id", "client_secret"],
  "additionalProperties": false,
  "properties": {
    "client_id": {"type": "string", "title": "App ID (client id)", "minLength": 1},
    "client_secret": {"type": "string", "title": "Secret key (client secret)", "minLength": 1, "x-secret": true},
    "webhook_secret": {"type": "string", "title": "Webhook signing secret (default: the client secret)", "x-secret": true},
    "sandbox": {"type": "boolean", "title": "Use the sandbox (sandbox.cashfree.com)"}
  }
}`

var info = connector.Info{
	Version:      "0.1.0",
	DisplayName:  "Cashfree",
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapSettle},
	ConfigSchema: json.RawMessage(configSchema),
	Currencies:   []string{"INR"},
	Methods:      []string{"card", "upi", "netbanking", "wallet", "emi", "paylater"},
	// TODO(spec): Cashfree's documented limits are per endpoint; stay modest.
	RateLimit: connector.RateLimit{RequestsPerSecond: 10, Burst: 10},
}

func init() { connector.Register("cashfree", New, info) }

type config struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	Sandbox       bool   `json:"sandbox"`
}

type cashfree struct {
	cfg  config
	deps connector.Deps
	api  *client
}

// New is the connector.Factory for Cashfree.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("cashfree: invalid config: %w", err)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("cashfree: client_id and client_secret are required")
	}
	if cfg.WebhookSecret == "" {
		// TODO(spec): Cashfree signs webhooks with the same secret key as the API.
		cfg.WebhookSecret = cfg.ClientSecret
	}
	hc := deps.HTTP
	if hc == nil {
		hc = connector.NewHTTPClient(connector.HTTPOptions{Logger: deps.Logger, Secrets: []string{cfg.ClientSecret, cfg.WebhookSecret}, RateLimit: info.RateLimit})
	}
	base := prodBase
	if cfg.Sandbox {
		base = sandboxBase
	}
	return &cashfree{cfg: cfg, deps: deps, api: &client{http: hc, base: base, id: cfg.ClientID, secret: cfg.ClientSecret}}, nil
}

func (c *cashfree) Info() connector.Info {
	i := info
	i.Name = "cashfree"
	return i
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// amount is a decimal amount as Cashfree sends it: a JSON number, though a
// quoted string and null are tolerated (real APIs differ from their docs).
type amount json.Number

func (a *amount) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" {
		s = ""
	}
	if strings.ContainsAny(s, "eE") { // 1e2: normalise to plain decimal
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return fmt.Errorf("cashfree: bad amount %q", s)
		}
		s = strings.TrimRight(strings.TrimRight(r.FloatString(6), "0"), ".")
	}
	*a = amount(s)
	return nil
}

func (a amount) minor(currency string) (int64, error) {
	s := strings.TrimSpace(string(a))
	if s == "" {
		return 0, nil
	}
	return money.ParseDecimal(s, currency)
}

func decimal(minor int64, currency string) (json.Number, error) {
	s, err := money.FormatDecimal(minor, currency)
	return json.Number(s), err
}

// customerID derives a stable Cashfree customer id (alphanumeric, underscore,
// hyphen) from what we know of the buyer.
func customerID(req connector.CheckoutRequest) string {
	key := strings.ToLower(req.Customer.Email) + "|" + req.Customer.Phone
	if key == "|" {
		key = req.PaymentID
	}
	sum := sha256.Sum256([]byte(key))
	return "c_" + hex.EncodeToString(sum[:])[:24]
}

// phone10 keeps the last ten digits of a phone number; Cashfree requires one.
//
// TODO(spec): the order API requires customer_phone. With none, a placeholder
// is sent so the order can be created; check how the live API treats it.
func phone10(p string) string {
	var digits []byte
	for i := 0; i < len(p); i++ {
		if p[i] >= '0' && p[i] <= '9' {
			digits = append(digits, p[i])
		}
	}
	if len(digits) >= 10 {
		return string(digits[len(digits)-10:])
	}
	return fallbackMSDN
}

type orderWire struct {
	CFOrderID        json.Number `json:"cf_order_id"`
	OrderID          string      `json:"order_id"`
	OrderStatus      string      `json:"order_status"`
	OrderAmount      amount      `json:"order_amount"`
	OrderCurrency    string      `json:"order_currency"`
	PaymentSessionID string      `json:"payment_session_id"`
	OrderExpiryTime  string      `json:"order_expiry_time"`
	CreatedAt        string      `json:"created_at"`
}

// CreateCheckout creates an order whose order_id is the attempt's provider_ref
// and returns the payment_session_id for Cashfree's JS checkout.
func (c *cashfree) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || len(req.ProviderRef) > maxOrderID || !idRE.MatchString(req.ProviderRef) {
		return connector.CheckoutResult{}, fmt.Errorf("cashfree: provider_ref must be 1..%d letters, digits, _ or - (it is the order_id)", maxOrderID)
	}
	if req.Currency != "INR" {
		return connector.CheckoutResult{}, fmt.Errorf("cashfree: currency %q is not supported (INR only)", req.Currency)
	}
	if req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("cashfree: amount must be positive")
	}
	amt, err := decimal(req.Amount, req.Currency)
	if err != nil {
		return connector.CheckoutResult{}, fmt.Errorf("cashfree: %w", err)
	}
	cust := map[string]any{"customer_id": customerID(req), "customer_phone": phone10(req.Customer.Phone)}
	if req.Customer.Email != "" {
		cust["customer_email"] = req.Customer.Email
	}
	if req.Customer.Name != "" {
		cust["customer_name"] = req.Customer.Name
	}
	in := map[string]any{
		"order_id": req.ProviderRef, "order_amount": amt, "order_currency": "INR", "customer_details": cust,
		"order_meta": map[string]string{
			// Cashfree replaces {order_id} in the return url.
			"return_url": withOrderID(c.deps.URLs.ReturnURL(req.ProviderRef)),
			"notify_url": c.deps.URLs.WebhookURL(),
		},
		"order_tags": map[string]string{"pay": req.PaymentID, "att": req.AttemptID},
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		if len(d) > 200 {
			d = d[:200]
		}
		in["order_note"] = d
	}
	var o orderWire
	if err := c.api.do(ctx, "POST", "/orders", in, &o, "order:"+req.ProviderRef); err != nil {
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Code != "order_already_exists" {
			return connector.CheckoutResult{}, err
		}
		// A retry after the idempotency window: adopt the order if it is ours.
		o = orderWire{}
		if err := c.api.do(ctx, "GET", "/orders/"+url.PathEscape(req.ProviderRef), nil, &o, ""); err != nil {
			return connector.CheckoutResult{}, err
		}
		if got, _ := o.OrderAmount.minor(req.Currency); got != req.Amount || o.OrderCurrency != req.Currency || o.OrderStatus != "ACTIVE" {
			return connector.CheckoutResult{}, fmt.Errorf("cashfree: order %s already exists with another amount, currency or state (%s)", req.ProviderRef, o.OrderStatus)
		}
	}
	if o.OrderID != req.ProviderRef || o.PaymentSessionID == "" {
		return connector.CheckoutResult{}, errors.New("cashfree: order response does not match the request (order_id or payment_session_id)")
	}
	mode := "production"
	if c.cfg.Sandbox {
		mode = "sandbox"
	}
	return connector.CheckoutResult{ProviderOrderID: o.OrderID, NextAction: connector.NextAction{Kind: connector.ActionSDK, ScriptURL: sdkScript,
		Options: map[string]any{"payment_session_id": o.PaymentSessionID, "mode": mode, "return_url": withOrderID(c.deps.URLs.ReturnURL(req.ProviderRef))}}}, nil
}

func withOrderID(u string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "order_id={order_id}"
}

// HandleReturn accepts the customer's return. Cashfree signs nothing on the
// redirect; the order_id it substitutes must be this attempt's own. The caller
// must still fetch the payment.
func (c *cashfree) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	if req.ProviderRef == "" {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	if id := req.Param("order_id"); id != "" && id != req.ProviderRef {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: req.ProviderRef, ProviderOrderID: req.ProviderRef}, nil
}
