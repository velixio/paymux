// Package zoho is the Zoho Payments (India) connector. It imports only the
// stdlib and pkg/.... There is no dispute API: disputes are manual.
//
// TODO(spec): re-verify against the live docs at sandbox time (the sandbox must
// be enabled by Zoho support): the widget script URL, the hosted-checkout URL
// scheme, and the webhook event names. Those are isolated in constants below.
package zoho

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

const (
	widgetScript     = "https://static.zohocdn.com/zpay/zpay-js/v1/zpayments.js"
	webhookTolerance = 5 * time.Minute
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["account_id", "client_id", "client_secret", "refresh_token", "webhook_secret"],
  "additionalProperties": false,
  "properties": {
    "account_id": {"type": "string", "title": "Zoho Payments account id", "minLength": 1},
    "client_id": {"type": "string", "title": "OAuth client id", "minLength": 1},
    "client_secret": {"type": "string", "title": "OAuth client secret", "minLength": 1, "x-secret": true},
    "refresh_token": {"type": "string", "title": "OAuth refresh token", "minLength": 1, "x-secret": true},
    "webhook_secret": {"type": "string", "title": "Webhook signing key", "minLength": 1, "x-secret": true},
    "data_center": {"type": "string", "title": "Zoho data centre TLD", "default": "in"},
    "widget_api_key": {"type": "string", "title": "Widget API key (enables the embedded widget)"},
    "hosted_checkout_url": {"type": "string", "format": "uri", "title": "Hosted checkout base URL (used when no widget key is set)"}
  }
}`

var info = connector.Info{
	Version:      "0.1.0",
	DisplayName:  "Zoho Payments",
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapMandateManager},
	ConfigSchema: json.RawMessage(configSchema),
	Currencies:   []string{"INR"},
	Methods:      []string{"upi", "card", "netbanking"},
	// UPI AutoPay mandates only; the issuer notifies the customer before each
	// debit, so there is no pre-debit API (NotifyPreDebit returns ErrUnsupported).
	SubscriptionModes:        []connector.BillingMode{connector.MerchantTriggered},
	MandateLimits:            []connector.MandateLimit{{Method: "upi", Currency: "INR"}},
	RefundsVerificationDebit: true, // TODO(spec): confirm Zoho reverses the verification debit itself
	MandateSetupAmount:       100,  // the Rs 1 verification debit of a registration without a charge due (trials)
	// Refunds are limited to 60/min; the client limiter applies that to every call.
	RateLimit: connector.RateLimit{RequestsPerSecond: 1, Burst: 10},
}

func init() { connector.Register("zoho", New, info) }

type config struct {
	AccountID         string `json:"account_id"`
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"client_secret"`
	RefreshToken      string `json:"refresh_token"`
	WebhookSecret     string `json:"webhook_secret"`
	DataCenter        string `json:"data_center"`
	WidgetAPIKey      string `json:"widget_api_key"`
	HostedCheckoutURL string `json:"hosted_checkout_url"`
}

func (c config) dc() string {
	if c.DataCenter == "" {
		return "in"
	}
	return c.DataCenter
}

type zoho struct {
	cfg  config
	deps connector.Deps
	api  *client
}

// New is the connector.Factory for Zoho Payments.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("zoho: invalid config: %w", err)
	}
	if cfg.AccountID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RefreshToken == "" || cfg.WebhookSecret == "" {
		return nil, errors.New("zoho: account_id, client_id, client_secret, refresh_token and webhook_secret are required")
	}
	if cfg.WidgetAPIKey == "" && cfg.HostedCheckoutURL == "" {
		return nil, errors.New("zoho: set widget_api_key (widget) or hosted_checkout_url (redirect)")
	}
	if deps.KV == nil {
		return nil, errors.New("zoho: Deps.KV is required (access token cache)")
	}
	hc := deps.HTTP
	if hc == nil {
		hc = connector.NewHTTPClient(connector.HTTPOptions{
			Logger: deps.Logger, RateLimit: info.RateLimit,
			Secrets: []string{cfg.ClientSecret, cfg.RefreshToken, cfg.WebhookSecret},
		})
	}
	return &zoho{cfg: cfg, deps: deps, api: &client{http: hc, kv: deps.KV, now: deps.Now, cfg: cfg}}, nil
}

func (z *zoho) Info() connector.Info {
	i := info
	i.Name = "zoho"
	return i
}

// number accepts a JSON number or numeric string.
type number string

func (n *number) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" {
		s = ""
	}
	*n = number(s)
	return nil
}

func (n number) minor() (int64, error) {
	if n == "" {
		return 0, nil
	}
	return money.ParseDecimal(string(n), "INR")
}

type paymentEntity struct {
	PaymentID string `json:"payment_id"`
	SessionID string `json:"payments_session_id"`
	Amount    number `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"`
	Method    string `json:"payment_method"`
	Reference string `json:"reference_number"`
	Message   string `json:"failure_reason"`
}

type refundEntity struct {
	RefundID  string `json:"refund_id"`
	PaymentID string `json:"payment_id"`
	Amount    number `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"`
	Reference string `json:"reference_number"`
}

func mapPaymentStatus(raw string) connector.PaymentStatus {
	switch strings.ToLower(raw) {
	case "succeeded", "success", "paid":
		return connector.PaymentSucceeded
	case "failed", "failure":
		return connector.PaymentFailed
	case "canceled", "cancelled":
		return connector.PaymentCanceled
	case "expired":
		return connector.PaymentExpired
	default: // "pending", "initiated", "processing", unknown
		return connector.PaymentProcessing
	}
}

func mapRefundStatus(raw string) connector.RefundStatus {
	switch strings.ToLower(raw) {
	case "succeeded", "success", "processed":
		return connector.RefundSucceeded
	case "failed", "failure":
		return connector.RefundFailed
	}
	return connector.RefundPending
}

func (p paymentEntity) snapshot(orderID string) (connector.PaymentSnapshot, error) {
	amt, err := p.Amount.minor()
	if err != nil {
		return connector.PaymentSnapshot{}, fmt.Errorf("zoho: payment amount: %w", err)
	}
	if p.SessionID != "" {
		orderID = p.SessionID
	}
	return connector.PaymentSnapshot{
		ProviderRef: p.Reference, ProviderOrderID: orderID, ProviderPaymentID: p.PaymentID,
		Status: mapPaymentStatus(p.Status), RawStatus: p.Status, Amount: amt,
		Currency: firstNonEmpty(p.Currency, "INR"), Method: p.Method, ErrorMessage: p.Message,
	}, nil
}

func (r refundEntity) snapshot() (connector.RefundSnapshot, error) {
	amt, err := r.Amount.minor()
	if err != nil {
		return connector.RefundSnapshot{}, fmt.Errorf("zoho: refund amount: %w", err)
	}
	return connector.RefundSnapshot{
		ProviderRef: r.Reference, ProviderRefundID: r.RefundID, ProviderPaymentID: r.PaymentID,
		Status: mapRefundStatus(r.Status), RawStatus: r.Status, Amount: amt, Currency: firstNonEmpty(r.Currency, "INR"),
	}, nil
}

// CreateCheckout creates a payment session and returns the widget (sdk) or
// hosted-checkout (redirect) action.
func (z *zoho) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.Currency != "INR" {
		return connector.CheckoutResult{}, fmt.Errorf("zoho: currency %q unsupported (INR only)", req.Currency)
	}
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("zoho: provider_ref and a positive amount are required")
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	in := map[string]any{
		"amount":           json.Number(amt),
		"currency":         "INR",
		"description":      firstNonEmpty(req.Description, "Payment"),
		"reference_number": req.ProviderRef,
		"return_url":       z.deps.URLs.ReturnURL(req.ProviderRef),
		"meta_data": []map[string]string{
			{"key": "pay", "value": req.PaymentID}, {"key": "att", "value": req.AttemptID}, {"key": "app", "value": req.AppID},
		},
	}
	var out struct {
		Session struct {
			ID string `json:"payments_session_id"`
		} `json:"payments_session"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/paymentsessions", nil, in, &out); err != nil {
		return connector.CheckoutResult{}, err
	}
	sid := out.Session.ID
	if sid == "" {
		return connector.CheckoutResult{}, errors.New("zoho: payment session response has no id")
	}
	res := connector.CheckoutResult{ProviderOrderID: sid}
	if z.cfg.WidgetAPIKey != "" {
		res.NextAction = connector.NextAction{Kind: connector.ActionSDK, ScriptURL: widgetScript, Options: map[string]any{
			"account_id": z.cfg.AccountID, "api_key": z.cfg.WidgetAPIKey, "domain": strings.ToUpper(z.cfg.dc()),
			"payments_session_id": sid, "amount": amt, "currency": "INR",
			"return_url": z.deps.URLs.ReturnURL(req.ProviderRef),
		}}
	} else {
		sep := "?"
		if strings.Contains(z.cfg.HostedCheckoutURL, "?") {
			sep = "&"
		}
		res.NextAction = connector.NextAction{Kind: connector.ActionRedirect,
			URL: z.cfg.HostedCheckoutURL + sep + "payments_session_id=" + url.QueryEscape(sid)}
	}
	return res, nil
}

// HandleReturn: Zoho signs nothing on the browser return, so there is nothing
// to verify. It only echoes the ids; the caller must fetch the truth (spec 6.3
// step 3) and must never trust these ids without it.
func (z *zoho) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	res := connector.ReturnResult{
		ProviderRef: req.ProviderRef, ProviderPaymentID: req.Param("payment_id"),
		ProviderOrderID: req.Param("payments_session_id"),
	}
	if res.ProviderPaymentID == "" && res.ProviderOrderID == "" {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return res, nil
}

// VerifyWebhook checks X-Zoho-Webhook-Signature: t=<unix s>,v=<hex>, where v is
// HMAC-SHA256(secret, t + "." + body), and rejects timestamps more than 5
// minutes from the receive time.
func (z *zoho) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	var t string
	var sigs []string
	for _, part := range strings.Split(req.Header("X-Zoho-Webhook-Signature"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			t = v
		case "v", "v1":
			sigs = append(sigs, v)
		}
	}
	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil || len(sigs) == 0 {
		return connector.ErrInvalidSignature
	}
	now := z.deps.Now()
	if req.ReceivedAt > 0 {
		now = time.UnixMilli(req.ReceivedAt)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > webhookTolerance || d < -webhookTolerance {
		return connector.ErrInvalidSignature
	}
	m := hmac.New(sha256.New, []byte(z.cfg.WebhookSecret))
	m.Write([]byte(t + "."))
	m.Write(req.Body)
	want := m.Sum(nil)
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(want, got) {
			return nil
		}
	}
	return connector.ErrInvalidSignature
}

// ParseWebhook maps payment.* and refund.* events; others are ignored.
func (z *zoho) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var ev struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		Payload   struct {
			Payment *paymentEntity `json:"payment"`
			Refund  *refundEntity  `json:"refund"`
			Mandate *mandateEntity `json:"mandate"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("zoho: bad webhook body: %w", err)
	}
	if ev.EventID == "" {
		return nil, errors.New("zoho: webhook has no event_id")
	}
	n := connector.Notification{EventID: ev.EventID, EventType: ev.EventType, OccurredAt: req.ReceivedAt}
	switch {
	case strings.HasPrefix(ev.EventType, "payment.") && ev.Payload.Payment != nil:
		s, err := ev.Payload.Payment.snapshot("")
		if err != nil {
			return nil, err
		}
		n.Kind, n.Payment = connector.NotifyPayment, &s
	case strings.HasPrefix(ev.EventType, "refund.") && ev.Payload.Refund != nil:
		s, err := ev.Payload.Refund.snapshot()
		if err != nil {
			return nil, err
		}
		n.Kind, n.Refund = connector.NotifyRefund, &s
	case strings.HasPrefix(ev.EventType, "mandate.") && ev.Payload.Mandate != nil:
		s, err := ev.Payload.Mandate.snapshot()
		if err != nil {
			return nil, err
		}
		n.Kind, n.Mandate = connector.NotifyMandate, &s
	default:
		return nil, nil
	}
	if err := n.Validate(); err != nil { // e.g. an entity with no id (found by fuzzing)
		return nil, fmt.Errorf("zoho: %w", err)
	}
	return []connector.Notification{n}, nil
}

// FetchPayment reads a payment by id, or a payment session and its best payment.
func (z *zoho) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	switch {
	case ref.ProviderPaymentID != "":
		var out struct {
			Payment paymentEntity `json:"payment"`
		}
		if err := z.api.do(ctx, "GET", "/api/v1/payments/"+url.PathEscape(ref.ProviderPaymentID), nil, nil, &out); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		return out.Payment.snapshot(ref.ProviderOrderID)
	case ref.ProviderOrderID != "":
		var out struct {
			Session struct {
				ID       string          `json:"payments_session_id"`
				Status   string          `json:"status"`
				Amount   number          `json:"amount"`
				Currency string          `json:"currency"`
				Payments []paymentEntity `json:"payments"`
			} `json:"payments_session"`
		}
		if err := z.api.do(ctx, "GET", "/api/v1/paymentsessions/"+url.PathEscape(ref.ProviderOrderID), nil, nil, &out); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		s := out.Session
		if len(s.Payments) == 0 {
			amt, err := s.Amount.minor()
			if err != nil {
				return connector.PaymentSnapshot{}, err
			}
			st := connector.PaymentRequiresAction
			if mapPaymentStatus(s.Status) == connector.PaymentExpired {
				st = connector.PaymentExpired
			}
			return connector.PaymentSnapshot{ProviderRef: ref.ProviderRef, ProviderOrderID: s.ID, Status: st,
				RawStatus: s.Status, Amount: amt, Currency: firstNonEmpty(s.Currency, "INR")}, nil
		}
		best := s.Payments[0]
		for _, p := range s.Payments[1:] {
			if rank(p.Status) > rank(best.Status) {
				best = p
			}
		}
		snap, err := best.snapshot(s.ID)
		if snap.ProviderRef == "" {
			snap.ProviderRef = ref.ProviderRef
		}
		return snap, err
	}
	return connector.PaymentSnapshot{}, errors.New("zoho: PaymentRef has no id")
}

func rank(status string) int {
	switch mapPaymentStatus(status) {
	case connector.PaymentSucceeded:
		return 3
	case connector.PaymentProcessing:
		return 2
	}
	return 1
}

// Refund creates a refund; the refund provider_ref is sent as reference_number.
func (z *zoho) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	if req.ProviderPaymentID == "" || req.Amount <= 0 {
		return connector.RefundSnapshot{}, errors.New("zoho: refund needs a payment id and a positive amount")
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	in := map[string]any{"amount": json.Number(amt), "reason": req.Reason, "reference_number": req.ProviderRef}
	var out struct {
		Refund refundEntity `json:"refund"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/payments/"+url.PathEscape(req.ProviderPaymentID)+"/refunds", nil, in, &out); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 400 {
			return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: ae.Message}
		}
		return connector.RefundSnapshot{}, err
	}
	s, err := out.Refund.snapshot()
	if s.ProviderRef == "" {
		s.ProviderRef = req.ProviderRef
	}
	if s.ProviderPaymentID == "" {
		s.ProviderPaymentID = req.ProviderPaymentID
	}
	return s, err
}

// FetchRefund reads a refund by its provider id.
func (z *zoho) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	if ref.ProviderRefundID == "" {
		return connector.RefundSnapshot{}, errors.New("zoho: RefundRef needs ProviderRefundID")
	}
	var out struct {
		Refund refundEntity `json:"refund"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/refunds/"+url.PathEscape(ref.ProviderRefundID), nil, nil, &out); err != nil {
		return connector.RefundSnapshot{}, err
	}
	return out.Refund.snapshot()
}

var _ connector.Refunder = (*zoho)(nil)
