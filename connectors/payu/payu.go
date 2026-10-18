// Package payu is the PayU India connector (hosted form_post checkout,
// reverse-hash verification, postservice API). It imports only the stdlib and
// pkg/....
//
// Verified against the PayU test environment except for the redirect back (the
// test merchant's response page timed out, so the reverse hash was not checked
// against a real PayU response) and for mandates; see docs/provider-verification.md.
package payu

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

const maxRefundTokenLen = 23 // PayU limit for cancel_refund_transaction var2

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["key", "salt"],
  "additionalProperties": false,
  "properties": {
    "key": {"type": "string", "title": "Merchant key", "minLength": 1},
    "salt": {"type": "string", "title": "Merchant salt (V1)", "minLength": 1, "x-secret": true},
    "test_mode": {"type": "boolean", "title": "Use test.payu.in", "default": false},
    "chargeback_token": {"type": "string", "title": "Chargeback API token (Chargeback dashboard, profile, tokens; for disputes)", "x-secret": true},
    "chargeback_merchant_id": {"type": "string", "title": "PayU merchant id for the Chargeback API (for disputes)"}
  }
}`

var info = connector.Info{
	Version:      "0.1.0",
	DisplayName:  "PayU India",
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapList, connector.CapMandateManager, connector.CapDisputeRead, connector.CapDisputeRespond},
	ConfigSchema: json.RawMessage(configSchema),
	Currencies:   []string{"INR"},
	Methods:      []string{"card", "upi", "netbanking", "wallet", "emi"},
	// Recurring: standing instructions on cards, UPI AutoPay and net-banking
	// e-mandates; Paymux triggers every debit. PayU wants a pre-debit notice
	// for every debit attempt, retries included. The e-NACH ceiling is Rs 1 crore.
	SubscriptionModes:        []connector.BillingMode{connector.MerchantTriggered},
	PreDebitPerAttempt:       true,
	RefundsVerificationDebit: true, // TODO(spec): PayU reverses the Rs 1 verification debit itself; confirm at sandbox time
	MandateSetupAmount:       100,  // the Rs 1 verification debit of a registration without a charge due (trials)
	MandateLimits: []connector.MandateLimit{{Method: "upi", Currency: "INR"}, {Method: "card", Currency: "INR"},
		{Method: "emandate", Currency: "INR", MaxAmount: 1_000_000_000}},
	// Disputes come from the Chargeback API (chargebacks.go), which needs its own
	// token; without it the dispute calls say so.
	RateLimit: connector.RateLimit{RequestsPerSecond: 10, Burst: 10},
}

func init() { connector.Register("payu", New, info) }

type config struct {
	Key      string `json:"key"`
	Salt     string `json:"salt"`
	TestMode bool   `json:"test_mode"`
	// The Chargeback API is a separate service with its own token.
	ChargebackToken      string `json:"chargeback_token"`
	ChargebackMerchantID string `json:"chargeback_merchant_id"`
}

type payu struct {
	cfg  config
	deps connector.Deps
	api  *client
	seq  atomic.Uint64
}

// New is the connector.Factory for PayU.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("payu: invalid config: %w", err)
	}
	if cfg.Key == "" || cfg.Salt == "" {
		return nil, errors.New("payu: key and salt are required")
	}
	hc := deps.HTTP
	if hc == nil {
		hc = connector.NewHTTPClient(connector.HTTPOptions{Logger: deps.Logger, Secrets: []string{cfg.Salt}, RateLimit: info.RateLimit})
	}
	return &payu{cfg: cfg, deps: deps, api: &client{http: hc, key: cfg.Key, salt: cfg.Salt, test: cfg.TestMode}}, nil
}

func (p *payu) Info() connector.Info {
	i := info
	i.Name = "payu"
	return i
}

func sha512Hex(parts ...string) string {
	sum := sha512.Sum512([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// requestHash is sha512(key|txnid|amount|productinfo|firstname|email|udf1|udf2|udf3|udf4|udf5||||||SALT).
func requestHash(key, salt, txnid, amount, productinfo, firstname, email string, udf [5]string) string {
	return sha512Hex(key, txnid, amount, productinfo, firstname, email,
		udf[0], udf[1], udf[2], udf[3], udf[4], "", "", "", "", "", salt)
}

// reverseHash is sha512([additionalCharges|]SALT|status||||||udf5|udf4|udf3|udf2|udf1|email|firstname|productinfo|amount|txnid|key),
// computed from the fields PayU sends back.
func reverseHash(salt string, f url.Values) string {
	parts := []string{salt, f.Get("status"), "", "", "", "", "",
		f.Get("udf5"), f.Get("udf4"), f.Get("udf3"), f.Get("udf2"), f.Get("udf1"),
		f.Get("email"), f.Get("firstname"), f.Get("productinfo"), f.Get("amount"), f.Get("txnid"), f.Get("key")}
	if ac := additionalCharges(f); ac != "" {
		parts = append([]string{ac}, parts...)
	}
	return sha512Hex(parts...)
}

// PayU sends the parameter as additionalCharges; additional_charges is accepted too.
func additionalCharges(f url.Values) string {
	if v := f.Get("additionalCharges"); v != "" {
		return v
	}
	return f.Get("additional_charges")
}

// verifyReverse checks the hash field of a PayU response/webhook in constant time.
func (p *payu) verifyReverse(f url.Values) bool {
	got := strings.ToLower(f.Get("hash"))
	if got == "" || f.Get("key") != p.cfg.Key {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(reverseHash(p.cfg.Salt, f))) == 1
}

// CreateCheckout builds the auto-submitting form fields; no API call is made.
func (p *payu) CreateCheckout(_ context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.Currency != "INR" {
		return connector.CheckoutResult{}, fmt.Errorf("payu: currency %q unsupported (INR only)", req.Currency)
	}
	if req.ProviderRef == "" {
		return connector.CheckoutResult{}, errors.New("payu: provider_ref (txnid) required")
	}
	amount, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil || req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("payu: amount must be positive")
	}
	// PayU rejects empty productinfo/firstname/email, but billing details are
	// optional in Paymux, so fall back to neutral placeholders.
	productinfo := orDefault(req.Description, "Payment")
	firstname := orDefault(req.Customer.Name, "Customer")
	email := orDefault(req.Customer.Email, "noreply@example.com")
	ret := p.deps.URLs.ReturnURL(req.ProviderRef)
	udf := [5]string{req.AttemptID}
	fields := map[string]string{
		"key": p.cfg.Key, "txnid": req.ProviderRef, "amount": amount,
		"productinfo": productinfo, "firstname": firstname, "email": email, "phone": req.Customer.Phone,
		"surl": ret, "furl": ret, "udf1": udf[0],
		"service_provider": "payu_paisa",
		"hash":             requestHash(p.cfg.Key, p.cfg.Salt, req.ProviderRef, amount, productinfo, firstname, email, udf),
	}
	return connector.CheckoutResult{NextAction: connector.NextAction{
		Kind: connector.ActionFormPost, URL: p.api.paymentURL(), Fields: fields,
	}}, nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

// formOf merges form, query and body parameters of a return.
func formOf(r connector.ReturnRequest) url.Values {
	v := url.Values{}
	if len(r.Body) > 0 {
		if b, err := url.ParseQuery(string(r.Body)); err == nil {
			v = b
		}
	}
	for _, m := range []map[string][]string{r.Query, r.Form} {
		for k, vs := range m {
			v[k] = vs
		}
	}
	return v
}

// HandleReturn verifies the reverse hash of the surl/furl POST.
func (p *payu) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	f := formOf(req)
	if !p.verifyReverse(f) || f.Get("txnid") == "" || (req.ProviderRef != "" && f.Get("txnid") != req.ProviderRef) {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: f.Get("txnid"), ProviderPaymentID: f.Get("mihpayid")}, nil
}

var (
	_ connector.Refunder = (*payu)(nil)
	_ connector.Lister   = (*payu)(nil)
)
