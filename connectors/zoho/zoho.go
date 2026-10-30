// Package zoho is the Zoho Payments (India) connector. It imports only the
// stdlib and pkg/.... There is no dispute API: disputes are manual.
//
// Verified against the live Zoho Payments API (see docs/provider-verification.md):
// token refresh, payment sessions, payments, refunds, error codes. Not verified:
// a completed widget payment, successful refunds, live webhook deliveries,
// mandates (the connector's mandate endpoints do not match Zoho's real flow).
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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

const (
	widgetScript     = "https://static.zohocdn.com/zpay/zpay-js/v1/zpayments.js"
	webhookTolerance = 5 * time.Minute
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["account_id", "client_id", "client_secret", "webhook_secret"],
  "additionalProperties": false,
  "properties": {
    "account_id": {"type": "string", "title": "Zoho Payments account id", "minLength": 1},
    "client_id": {"type": "string", "title": "OAuth client id", "minLength": 1, "x-oauth": "client_id"},
    "client_secret": {"type": "string", "title": "OAuth client secret", "minLength": 1, "x-secret": true, "x-oauth": "client_secret"},
    "refresh_token": {"type": "string", "title": "OAuth refresh token (filled in by Connect; leave empty until then)", "x-secret": true, "x-oauth": "refresh_token"},
    "oauth_scopes": {"type": "string", "title": "Scopes granted when the account was connected", "x-hidden": true, "x-oauth": "scopes"},
    "webhook_secret": {"type": "string", "title": "Webhook signing key", "minLength": 1, "x-secret": true, "x-webhook-secret": true, "x-generate": true},
    "data_center": {"type": "string", "title": "Zoho data centre TLD", "default": "in"},
    "widget_api_key": {"type": "string", "title": "Widget API key (enables the embedded widget)"},
    "hosted_checkout_url": {"type": "string", "format": "uri", "title": "Hosted checkout base URL (used when no widget key is set)"},
    "sandbox": {"type": "boolean", "title": "Use Zoho's sandbox (paymentssandbox.zoho.*; needs sandbox credentials)"}
  }
}`

// zohoHosts is every Zoho data centre's domain as a wildcard origin.
func zohoHosts() []string {
	var out []string
	for _, d := range dataCenters {
		out = append(out, "https://*."+d)
	}
	slices.Sort(out)
	return out
}

var info = connector.Info{
	Version:     "0.1.0",
	DisplayName: "Zoho Payments",
	// The widget script comes from the Zoho CDN; the widget then frames and calls
	// Zoho's own domain of the account's data centre (every one is listed).
	CheckoutHosts: &connector.CheckoutHosts{
		Script:  []string{"https://static.zohocdn.com"},
		Frame:   zohoHosts(),
		Connect: zohoHosts(),
		Img:     []string{"https://static.zohocdn.com"},
	},
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapMandateManager, connector.CapOAuth},
	ConfigSchema: json.RawMessage(configSchema),
	Currencies:   []string{"INR"},
	Methods:      []string{"upi", "card", "netbanking"},
	// UPI AutoPay mandates only; the issuer notifies the customer before each
	// debit, so there is no pre-debit API (NotifyPreDebit returns ErrUnsupported).
	SubscriptionModes: []connector.BillingMode{connector.MerchantTriggered},
	// RBI: an unattended UPI AutoPay debit is capped at Rs 15,000.
	MandateLimits: []connector.MandateLimit{{Method: "upi", Currency: "INR", MaxAmount: 1_500_000}},
	// Each execution session needs its own notice, retries included.
	PreDebitPerAttempt: true,
	// Zoho's help pages: the Rs 1 that authorises a future-dated mandate "will be
	// settled in your next payout", so Zoho does not reverse it; Paymux refunds it.
	RefundsVerificationDebit: false,
	MandateSetupAmount:       100, // the Rs 1 verification debit of a registration without a charge due (trials)
	// Rs 1, as the verification debit shows Zoho takes; Zoho publishes no other minimum (see docs/provider-verification.md).
	MinChargeMinor: map[string]int64{"INR": 100},
	// Refunds are limited to 60/min; the client limiter applies that to every call.
	RateLimit: connector.RateLimit{RequestsPerSecond: 1, Burst: 10},
}

func init() { connector.Register("zoho", New, info) }

type config struct {
	AccountID         string `json:"account_id"`
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"client_secret"`
	RefreshToken      string `json:"refresh_token"`
	OAuthScopes       string `json:"oauth_scopes"`
	WebhookSecret     string `json:"webhook_secret"`
	DataCenter        string `json:"data_center"`
	WidgetAPIKey      string `json:"widget_api_key"`
	HostedCheckoutURL string `json:"hosted_checkout_url"`
	Sandbox           bool   `json:"sandbox"`
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
	if cfg.AccountID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.WebhookSecret == "" {
		return nil, errors.New("zoho: account_id, client_id, client_secret and webhook_secret are required")
	}
	// The data centre builds the hosts the connector calls: only Zoho's are accepted.
	if _, ok := cfg.domain(); !ok {
		return nil, fmt.Errorf("zoho: data_center %q is not a Zoho data centre", cfg.DataCenter)
	}
	// An account without a refresh token is constructible: the operator creates
	// it, then connects it with Zoho. Until then its API calls say so.
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
			Secrets: nonEmpty(cfg.ClientSecret, cfg.RefreshToken, cfg.WebhookSecret),
		})
	}
	return &zoho{cfg: cfg, deps: deps, api: &client{http: hc, kv: deps.KV, now: deps.Now, cfg: cfg}}, nil
}

// CheckoutHosts adds the account's hosted checkout page (an operator-set URL)
// to the redirect targets; the widget and its hosts are the same for everyone.
func (z *zoho) CheckoutHosts() connector.CheckoutHosts {
	h := *info.CheckoutHosts
	if u, err := url.Parse(z.cfg.HostedCheckoutURL); err == nil && u.Scheme == "https" && u.Host != "" {
		h.Redirect = []string{"https://" + strings.ToLower(u.Host)}
	}
	return h
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

// methodField is payment_method: an object ({"type":"upi","upi":{...}}) in the
// real API, a plain string in older docs.
type methodField struct {
	Type string
	// MandateID is set on the payment that registered or was made through a UPI mandate.
	MandateID string
}

func (m *methodField) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*m = methodField{Type: s}
		return nil
	}
	var o struct {
		Type      string `json:"type"`
		MandateID string `json:"mandate_id"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		*m = methodField{}
		return nil // an unexpected shape must not fail the whole payment
	}
	*m = methodField{Type: o.Type, MandateID: o.MandateID}
	return nil
}

// methodName maps Zoho's payment method types to the names Paymux uses.
func methodName(t methodField) string {
	switch strings.ToLower(t.Type) {
	case "net_banking", "netbanking":
		return "netbanking"
	}
	return strings.ToLower(t.Type)
}

type metaItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// metaRef is the key under which Paymux's own reference rides on a refund.
const metaRef = "paymux_ref"

type paymentEntity struct {
	PaymentID      string         `json:"payment_id"`
	SessionID      string         `json:"payments_session_id"`
	Amount         number         `json:"amount"`
	AmountRefunded number         `json:"amount_refunded"`
	Fee            number         `json:"fee_amount"`
	Tax            number         `json:"net_tax_amount"`
	Currency       string         `json:"currency"`
	Status         string         `json:"status"`
	Method         methodField    `json:"payment_method"`
	Reference      string         `json:"reference_number"`
	Message        string         `json:"failure_reason"`
	Date           int64          `json:"date"` // unix seconds
	Refunds        []refundEntity `json:"refunds"`
}

type refundEntity struct {
	RefundID    string     `json:"refund_id"`
	PaymentID   string     `json:"payment_id"`
	Amount      number     `json:"amount"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status"`
	Reference   string     `json:"reference_number"`
	Description string     `json:"description"`
	Date        int64      `json:"date"`
	MetaData    []metaItem `json:"meta_data"`
}

// ref is the Paymux reference of a refund: the one stored in its meta_data when
// the refund was made through Paymux, else Zoho's own reference_number.
func (r refundEntity) ref() string {
	for _, m := range r.MetaData {
		if m.Key == metaRef && m.Value != "" {
			return m.Value
		}
	}
	return r.Reference
}

func mapPaymentStatus(raw string) connector.PaymentStatus {
	switch strings.ToLower(raw) {
	case "succeeded", "success", "paid", "refunded", "partially_refunded": // a refunded payment was paid
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
	case "failed", "failure", "canceled", "cancelled":
		return connector.RefundFailed
	}
	return connector.RefundPending // initiated, pending
}

func (p paymentEntity) snapshot(orderID string) (connector.PaymentSnapshot, error) {
	amt, err := p.Amount.minor()
	if err != nil {
		return connector.PaymentSnapshot{}, fmt.Errorf("zoho: payment amount: %w", err)
	}
	if p.SessionID != "" {
		orderID = p.SessionID
	}
	snap := connector.PaymentSnapshot{
		ProviderRef: p.Reference, ProviderOrderID: orderID, ProviderPaymentID: p.PaymentID,
		Status: mapPaymentStatus(p.Status), RawStatus: p.Status, Amount: amt,
		Currency: firstNonEmpty(p.Currency, "INR"), Method: methodName(p.Method), ErrorMessage: p.Message,
	}
	if p.Date > 0 {
		snap.CreatedAt = p.Date * 1000
	}
	snap.AmountRefunded, _ = p.AmountRefunded.minor()
	if strings.EqualFold(p.Status, "refunded") && snap.AmountRefunded == 0 {
		snap.AmountRefunded = amt
	}
	snap.Fee, _ = p.Fee.minor()
	snap.Tax, _ = p.Tax.minor()
	return snap, nil
}

func (r refundEntity) snapshot() (connector.RefundSnapshot, error) {
	amt, err := r.Amount.minor()
	if err != nil {
		return connector.RefundSnapshot{}, fmt.Errorf("zoho: refund amount: %w", err)
	}
	return connector.RefundSnapshot{
		ProviderRef: r.ref(), ProviderRefundID: r.RefundID, ProviderPaymentID: r.PaymentID,
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
	if len(req.ProviderRef) > 50 {
		return connector.CheckoutResult{}, errors.New("zoho: provider_ref is the reference_number, at most 50 characters")
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
		"meta_data": []map[string]string{
			{"key": "pay", "value": req.PaymentID}, {"key": "att", "value": req.AttemptID}, {"key": "app", "value": req.AppID},
		},
	}
	var out struct {
		Session struct {
			ID         string `json:"payments_session_id"`
			ExpiryTime int64  `json:"expiry_time"` // unix seconds
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
	if out.Session.ExpiryTime > 0 {
		res.ExpiresAt = out.Session.ExpiryTime * 1000
	}
	if z.cfg.WidgetAPIKey != "" {
		res.NextAction = connector.NextAction{Kind: connector.ActionSDK, ScriptURL: widgetScript, Options: map[string]any{
			"account_id": z.cfg.AccountID, "api_key": z.cfg.WidgetAPIKey, "domain": strings.ToUpper(z.cfg.dc()),
			"payments_session_id": sid, "amount": amt, "currency": "INR",
			"description": firstNonEmpty(req.Description, "Payment"), // required by requestPaymentMethod
			"return_url":  z.deps.URLs.ReturnURL(req.ProviderRef),
		}}
		if z.cfg.Sandbox {
			res.NextAction.Options["is_test_mode"] = true
		}
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
	// Zoho's t is in milliseconds (its documented example is 1734340423138); a
	// seconds value is accepted too.
	at := time.Unix(ts, 0)
	if ts > 1e11 {
		at = time.UnixMilli(ts)
	}
	if d := now.Sub(at); d > webhookTolerance || d < -webhookTolerance {
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
	// The envelope: event_id is a number (mandate events also carry event_id_str),
	// event_time is in milliseconds, the entity is under event_object.
	var ev struct {
		EventID    json.RawMessage `json:"event_id"`
		EventIDStr string          `json:"event_id_str"`
		EventType  string          `json:"event_type"`
		EventTime  int64           `json:"event_time"`
		Payload    struct {
			Payment *paymentEntity `json:"payment"`
			Refund  *refundEntity  `json:"refund"`
			Mandate *mandateEntity `json:"mandate"`
		} `json:"event_object"`
	}
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("zoho: bad webhook body: %w", err)
	}
	eventID := firstNonEmpty(ev.EventIDStr, strings.Trim(string(ev.EventID), `" `))
	if eventID == "" || eventID == "null" {
		return nil, errors.New("zoho: webhook has no event_id")
	}
	n := connector.Notification{EventID: eventID, EventType: ev.EventType, OccurredAt: req.ReceivedAt}
	if ev.EventTime > 0 {
		n.OccurredAt = ev.EventTime
	}
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
		// The payment id may have come from outside (the return URL): a
		// payment that names another reference is not the one we asked about.
		if got := out.Payment.Reference; got != "" && ref.ProviderRef != "" && got != ref.ProviderRef {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s belongs to another reference", connector.ErrNotFound, ref.ProviderPaymentID)
		}
		if got := out.Payment.SessionID; got != "" && ref.ProviderOrderID != "" && got != ref.ProviderOrderID {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s belongs to another session", connector.ErrNotFound, ref.ProviderPaymentID)
		}
		// Not contradicting us is not enough: a payment that names no session and
		// no reference (a payment link's, an invoice's) is not ours either.
		if (ref.ProviderRef != "" || ref.ProviderOrderID != "") &&
			!(out.Payment.Reference != "" && out.Payment.Reference == ref.ProviderRef) &&
			!(out.Payment.SessionID != "" && out.Payment.SessionID == ref.ProviderOrderID) {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s names neither our reference nor our session", connector.ErrNotFound, ref.ProviderPaymentID)
		}
		return out.Payment.snapshot(ref.ProviderOrderID)
	case ref.ProviderOrderID != "":
		var out struct {
			Session struct {
				ID        string `json:"payments_session_id"`
				Status    string `json:"status"`
				Amount    number `json:"amount"`
				Currency  string `json:"currency"`
				Reference string `json:"reference_number"`
				Payments  []struct {
					PaymentID string `json:"payment_id"`
					Status    string `json:"status"`
				} `json:"payments"`
			} `json:"payments_session"`
		}
		if err := z.api.do(ctx, "GET", "/api/v1/paymentsessions/"+url.PathEscape(ref.ProviderOrderID), nil, nil, &out); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		s := out.Session
		// The session carries our reference_number: another reference is not ours.
		if s.Reference != "" && ref.ProviderRef != "" && s.Reference != ref.ProviderRef {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: session %s belongs to another reference", connector.ErrNotFound, ref.ProviderOrderID)
		}
		if len(s.Payments) == 0 {
			amt, err := s.Amount.minor()
			if err != nil {
				return connector.PaymentSnapshot{}, err
			}
			st := connector.PaymentRequiresAction
			if mapPaymentStatus(s.Status) == connector.PaymentExpired {
				st = connector.PaymentExpired
			}
			return connector.PaymentSnapshot{ProviderRef: firstNonEmpty(s.Reference, ref.ProviderRef), ProviderOrderID: s.ID, Status: st,
				RawStatus: s.Status, Amount: amt, Currency: firstNonEmpty(s.Currency, "INR")}, nil
		}
		// The session lists only ids and statuses: read the best payment itself.
		best := s.Payments[0]
		for _, p := range s.Payments[1:] {
			if rank(p.Status) > rank(best.Status) {
				best = p
			}
		}
		var pay struct {
			Payment paymentEntity `json:"payment"`
		}
		if err := z.api.do(ctx, "GET", "/api/v1/payments/"+url.PathEscape(best.PaymentID), nil, nil, &pay); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		p := pay.Payment
		if (p.SessionID != "" && p.SessionID != s.ID) || (p.Reference != "" && ref.ProviderRef != "" && p.Reference != ref.ProviderRef) {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s is not this session's", connector.ErrNotFound, best.PaymentID)
		}
		snap, err := p.snapshot(s.ID)
		if snap.ProviderRef == "" {
			snap.ProviderRef = firstNonEmpty(s.Reference, ref.ProviderRef)
		}
		return snap, err
	}
	// A debit made through a mandate is known by Paymux's reference only: its
	// execution session was remembered when it was made.
	if ref.ProviderRef != "" {
		if sid := z.kvGet(ctx, "zoho:execd:"+ref.ProviderRef); sid != "" {
			return z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: sid, ProviderRef: ref.ProviderRef})
		}
		if sid := z.kvGet(ctx, "zoho:msess:"+ref.ProviderRef); sid != "" {
			return z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: sid, ProviderRef: ref.ProviderRef})
		}
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

// notRefundableCodes are the codes of a refund Zoho refuses because of the
// payment (payment_already_refunded was seen live; the others are the refund
// failure reasons the documentation lists).
var notRefundableCodes = map[string]bool{
	"payment_already_refunded": true, "exceeds_payment_amount": true, "exceeds_balance_amount": true, "payment_already_disputed": true,
	"payment_not_captured": true, "expiry_time_exceeded": true, "insufficient_funds": true,
}

// Refund creates a refund; the refund provider_ref is stored in its meta_data.
func (z *zoho) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	if req.ProviderPaymentID == "" || req.Amount <= 0 {
		return connector.RefundSnapshot{}, errors.New("zoho: refund needs a payment id and a positive amount")
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	// Zoho documents no idempotency for refunds, so a retried refund is looked up
	// first: the payment lists its refunds with their meta_data, where Paymux's
	// reference was stored.
	var pay struct {
		Payment paymentEntity `json:"payment"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/payments/"+url.PathEscape(req.ProviderPaymentID), nil, nil, &pay); err != nil {
		return connector.RefundSnapshot{}, err
	}
	for _, r := range pay.Payment.Refunds {
		if r.ref() != req.ProviderRef {
			continue
		}
		s, err := r.snapshot()
		if err != nil {
			return connector.RefundSnapshot{}, err
		}
		if s.Amount != req.Amount {
			return connector.RefundSnapshot{}, fmt.Errorf("zoho: refund reference %s already used for %d, not %d", req.ProviderRef, s.Amount, req.Amount)
		}
		s.ProviderPaymentID = req.ProviderPaymentID
		return s, nil
	}
	// reason must be one of Zoho's values; a free text goes to the description.
	reason := "others"
	switch req.Reason {
	case "duplicate", "fraudulent", "requested_by_customer", "others":
		reason = req.Reason
	}
	in := map[string]any{"amount": json.Number(amt), "reason": reason, "type": "initiated_by_merchant",
		"meta_data": []metaItem{{Key: metaRef, Value: req.ProviderRef}}}
	if req.Reason != "" {
		in["description"] = req.Reason
	}
	var out struct {
		Refund refundEntity `json:"refund"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/payments/"+url.PathEscape(req.ProviderPaymentID)+"/refunds", nil, in, &out); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 400 && notRefundableCodes[ae.Code] {
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

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
