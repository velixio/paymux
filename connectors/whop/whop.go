// Package whop is the Whop connector. Whop is the merchant of record, so
// Paymux issues no invoice for these payments. It imports only the stdlib and
// pkg/....
//
// TODO(spec): re-verify against the live v1 API at sandbox time: payment
// status/substatus values, the payments filter used to look up a checkout
// configuration's payments, and the refund response shape.
package whop

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

const (
	apiBase          = "https://api.whop.com/api/v1"
	sandboxAPIBase   = "https://sandbox-api.whop.com/api/v1"
	webhookTolerance = 5 * time.Minute
)

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["api_key", "company_id", "webhook_secret"],
  "additionalProperties": false,
  "properties": {
    "api_key": {"type": "string", "title": "API key", "minLength": 1, "x-secret": true},
    "company_id": {"type": "string", "title": "Company id (biz_...)", "minLength": 1},
    "webhook_secret": {"type": "string", "title": "Webhook secret (ws_...)", "minLength": 1, "x-secret": true},
    "sandbox": {"type": "boolean", "title": "Use the Whop sandbox (sandbox.whop.com)", "default": false}
  }
}`

var info = connector.Info{
	Version:      "0.1.0",
	DisplayName:  "Whop",
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapSubscriptionManager},
	ConfigSchema: json.RawMessage(configSchema),
	// Two-decimal currencies only; Whop settles in the organisation currency,
	// so the core records the FX separately.
	Currencies: []string{"USD", "EUR", "GBP", "CAD", "AUD", "NZD", "SGD", "HKD", "CHF", "SEK", "NOK", "DKK", "PLN",
		"CZK", "BRL", "MXN", "ZAR", "AED", "SAR", "TRY", "THB", "MYR", "PHP", "IDR", "INR", "BDT", "PKR", "NGN", "KES", "EGP"},
	Methods:          []string{"card", "wallet"},
	MerchantOfRecord: true,
	// Recurring billing is Whop's: memberships on renewal plans (subscriptions.go).
	SubscriptionModes: []connector.BillingMode{connector.ProviderManaged},
	// TODO(spec): dispute evidence API is not built.
	RateLimit: connector.RateLimit{RequestsPerSecond: 2, Burst: 10},
}

func init() { connector.Register("whop", New, info) }

type config struct {
	APIKey        string `json:"api_key"`
	CompanyID     string `json:"company_id"`
	WebhookSecret string `json:"webhook_secret"`
	Sandbox       bool   `json:"sandbox"`
}

type whop struct {
	cfg  config
	deps connector.Deps
	http *http.Client
	key  []byte
}

// New is the connector.Factory for Whop.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("whop: invalid config: %w", err)
	}
	if cfg.APIKey == "" || cfg.CompanyID == "" || cfg.WebhookSecret == "" {
		return nil, errors.New("whop: api_key, company_id and webhook_secret are required")
	}
	hc := deps.HTTP
	if hc == nil {
		hc = connector.NewHTTPClient(connector.HTTPOptions{Logger: deps.Logger, Secrets: []string{cfg.APIKey, cfg.WebhookSecret}, RateLimit: info.RateLimit})
	}
	return &whop{cfg: cfg, deps: deps, http: hc, key: signingKey(cfg.WebhookSecret)}, nil
}

// signingKey derives the HMAC key from the configured secret.
//
// Standard Webhooks libraries base64-decode the key. Whop's own examples feed
// them base64(secret) of the dashboard's "ws_..." value, so the effective key is
// the raw secret bytes, "ws_" prefix included. We therefore use the secret raw.
// Only a literal Standard Webhooks "whsec_" secret is base64-decoded.
func signingKey(secret string) []byte {
	if rest, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if b, err := base64.StdEncoding.DecodeString(rest); err == nil {
			return b
		}
	}
	return []byte(secret)
}

func (w *whop) Info() connector.Info {
	i := info
	i.Name = "whop"
	return i
}

// base is the API host for this account: production or the Whop sandbox.
func (w *whop) base() string {
	if w.cfg.Sandbox {
		return sandboxAPIBase
	}
	return apiBase
}

func (w *whop) do(ctx context.Context, method, path string, query url.Values, in, out any, hdr map[string]string) error {
	u := w.base() + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %v", connector.ErrTemporary, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", connector.ErrTemporary, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("whop: decoding %s %s: %w", method, path, err)
		}
		return nil
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &env)
	ae := &apiError{Status: resp.StatusCode, Message: firstNonEmpty(env.Error.Message, env.Message)}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("%w: %w", connector.ErrInvalidCredentials, ae)
	case resp.StatusCode == 404:
		return fmt.Errorf("%w: %w", connector.ErrNotFound, ae)
	case resp.StatusCode == 429:
		after := 10 * time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return fmt.Errorf("%w: %w", connector.ErrRateLimited{RetryAfter: after}, ae)
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %w", connector.ErrTemporary, ae)
	}
	return ae
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("whop: HTTP %d: %s", e.Status, e.Message) }

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// number accepts a JSON number or numeric string.
type number string

func (n *number) UnmarshalJSON(b []byte) error {
	// Some endpoints return money as {"currency":"usd","amount":"10.00",...}.
	if t := strings.TrimSpace(string(b)); strings.HasPrefix(t, "{") {
		var obj struct {
			Amount json.RawMessage `json:"amount"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		if len(obj.Amount) == 0 {
			*n = ""
			return nil
		}
		return n.UnmarshalJSON(obj.Amount)
	}
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "null" {
		s = ""
	}
	*n = number(s)
	return nil
}

func minor(n number, cur string) (int64, error) {
	if n == "" {
		return 0, nil
	}
	v, err := money.ParseDecimal(string(n), cur)
	if err != nil {
		return 0, fmt.Errorf("whop: amount %q: %w", n, err)
	}
	return v, nil
}

type paymentEntity struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	Substatus      string            `json:"substatus"`
	Total          number            `json:"total"`
	Currency       string            `json:"currency"`
	RefundedAmount number            `json:"refunded_amount"`
	Metadata       map[string]string `json:"metadata"`
	CheckoutConfig string            `json:"checkout_configuration_id"`
	MembershipID   string            `json:"membership_id"`
	Membership     *struct {
		ID string `json:"id"`
	} `json:"membership"`
	PaymentMethod  string `json:"payment_method_type"`
	CreatedAt      string `json:"created_at"`
	FailureMessage string `json:"failure_message"`
}

type refundEntity struct {
	ID        string `json:"id"`
	PaymentID string `json:"payment_id"`
	Payment   *struct {
		ID string `json:"id"`
	} `json:"payment"`
	Amount    number `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func mapPaymentStatus(status, substatus string) connector.PaymentStatus {
	s := strings.ToLower(firstNonEmpty(substatus, status))
	switch s {
	case "succeeded", "paid", "refunded", "partially_refunded", "auto_refunded", "resolution_won":
		return connector.PaymentSucceeded
	case "failed", "uncollectible", "void", "canceled", "cancelled":
		return connector.PaymentFailed
	case "draft":
		return connector.PaymentRequiresAction
	default: // "pending", "open", unknown
		return connector.PaymentProcessing
	}
}

func mapRefundStatus(raw string) connector.RefundStatus {
	switch strings.ToLower(raw) {
	case "succeeded", "success", "refunded", "processed":
		return connector.RefundSucceeded
	case "failed", "canceled", "cancelled", "rejected":
		return connector.RefundFailed
	}
	return connector.RefundPending
}

func parseTime(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func (p paymentEntity) snapshot() (connector.PaymentSnapshot, error) {
	cur := strings.ToUpper(p.Currency)
	amt, err := minor(p.Total, firstNonEmpty(cur, "USD"))
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	ref, err := minor(p.RefundedAmount, firstNonEmpty(cur, "USD"))
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	return connector.PaymentSnapshot{
		ProviderRef: p.Metadata["ref"], ProviderOrderID: p.CheckoutConfig, ProviderPaymentID: p.ID,
		Status: mapPaymentStatus(p.Status, p.Substatus), RawStatus: firstNonEmpty(p.Substatus, p.Status),
		Amount: amt, Currency: cur, Method: p.PaymentMethod, AmountRefunded: ref,
		ErrorMessage: p.FailureMessage, CreatedAt: parseTime(p.CreatedAt),
	}, nil
}

func (r refundEntity) snapshot() (connector.RefundSnapshot, error) {
	cur := strings.ToUpper(r.Currency)
	amt, err := minor(r.Amount, firstNonEmpty(cur, "USD"))
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	pid := r.PaymentID
	if pid == "" && r.Payment != nil {
		pid = r.Payment.ID
	}
	return connector.RefundSnapshot{
		ProviderRefundID: r.ID, ProviderPaymentID: pid, Status: mapRefundStatus(r.Status), RawStatus: r.Status,
		Amount: amt, Currency: cur, CreatedAt: parseTime(r.CreatedAt),
	}, nil
}

// CreateCheckout creates a checkout configuration (inline one-time plan) and
// redirects the customer to its purchase_url. provider_ref travels in metadata
// so webhooks can be matched back.
func (w *whop) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("whop: provider_ref and a positive amount are required")
	}
	if _, err := money.Exponent(req.Currency); err != nil {
		return connector.CheckoutResult{}, fmt.Errorf("whop: %w", err)
	}
	price, err := money.FormatDecimal(req.Amount, req.Currency)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	in := map[string]any{
		"plan": map[string]any{
			"company_id": w.cfg.CompanyID, "plan_type": "one_time",
			"currency": strings.ToLower(req.Currency), "initial_price": json.Number(price),
			"product": oneTimeProduct(req.AppID, req.Description),
		},
		"metadata": map[string]string{"pay": req.PaymentID, "att": req.AttemptID, "app": req.AppID, "ref": req.ProviderRef},
	}
	w.setRedirect(in, req.ProviderRef)
	var out struct {
		ID          string `json:"id"`
		PurchaseURL string `json:"purchase_url"`
	}
	if err := w.do(ctx, "POST", "/checkout_configurations", nil, in, &out, nil); err != nil {
		return connector.CheckoutResult{}, err
	}
	if out.ID == "" || out.PurchaseURL == "" {
		return connector.CheckoutResult{}, errors.New("whop: checkout configuration response lacks id or purchase_url")
	}
	return connector.CheckoutResult{
		ProviderOrderID: out.ID,
		NextAction:      connector.NextAction{Kind: connector.ActionRedirect, URL: out.PurchaseURL},
	}, nil
}

// HandleReturn: Whop signs nothing on the redirect, so there is nothing to
// verify. It echoes any ids present; the caller fetches the truth and must not
// trust the redirect.
func (w *whop) HandleReturn(_ context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{
		ProviderRef:       req.ProviderRef,
		ProviderPaymentID: firstNonEmpty(req.Param("payment_id"), req.Param("receipt_id")),
	}, nil
}

// VerifyWebhook implements Standard Webhooks: webhook-id, webhook-timestamp and
// webhook-signature ("v1,<base64 HMAC-SHA256 of id.timestamp.body>", possibly
// several, space separated), with a 5 minute timestamp tolerance.
func (w *whop) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	id, ts, sigHdr := req.Header("webhook-id"), req.Header("webhook-timestamp"), req.Header("webhook-signature")
	if id == "" || ts == "" || sigHdr == "" {
		return connector.ErrInvalidSignature
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return connector.ErrInvalidSignature
	}
	now := w.deps.Now()
	if req.ReceivedAt > 0 {
		now = time.UnixMilli(req.ReceivedAt)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > webhookTolerance || d < -webhookTolerance {
		return connector.ErrInvalidSignature
	}
	m := hmac.New(sha256.New, w.key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(req.Body)
	want := m.Sum(nil)
	for _, part := range strings.Fields(sigHdr) {
		ver, sig, ok := strings.Cut(part, ",")
		if !ok || ver != "v1" {
			continue
		}
		if got, err := base64.StdEncoding.DecodeString(sig); err == nil && hmac.Equal(want, got) {
			return nil
		}
	}
	return connector.ErrInvalidSignature
}

// ParseWebhook maps payment.* and refund.* events; others are ignored.
func (w *whop) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	var ev struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(req.Body, &ev); err != nil {
		return nil, fmt.Errorf("whop: bad webhook body: %w", err)
	}
	id := firstNonEmpty(req.Header("webhook-id"), ev.ID)
	if id == "" {
		return nil, errors.New("whop: webhook has no id")
	}
	n := connector.Notification{EventID: id, EventType: ev.Type, OccurredAt: parseTime(ev.Timestamp)}
	switch {
	case strings.HasPrefix(ev.Type, "payment."):
		var p paymentEntity
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			return nil, fmt.Errorf("whop: %s data: %w", ev.Type, err)
		}
		s, err := p.snapshot()
		if err != nil {
			return nil, err
		}
		if p.Metadata["sub"] != "" {
			// A payment of a subscription checkout (the first charge or a
			// renewal): reported as a subscription notification carrying it.
			mid := p.MembershipID
			if mid == "" && p.Membership != nil {
				mid = p.Membership.ID
			}
			st := "active"
			if s.Status == connector.PaymentFailed {
				st = "past_due"
			}
			n.Kind = connector.NotifySubscription
			n.Subscription = &connector.SubscriptionSnapshot{ProviderSubscriptionID: mid, ProviderRef: p.Metadata["ref"], Status: st,
				RawStatus: s.RawStatus, Amount: s.Amount, Currency: s.Currency}
			n.Payment = &s
			break
		}
		n.Kind, n.Payment = connector.NotifyPayment, &s
	case strings.HasPrefix(ev.Type, "membership."):
		var m membershipEntity
		if err := json.Unmarshal(ev.Data, &m); err != nil {
			return nil, fmt.Errorf("whop: %s data: %w", ev.Type, err)
		}
		if m.ID == "" {
			return nil, fmt.Errorf("whop: %s without a membership id", ev.Type)
		}
		snap := m.snapshot()
		n.Kind, n.Subscription = connector.NotifySubscription, &snap
	case strings.HasPrefix(ev.Type, "refund."):
		var r refundEntity
		if err := json.Unmarshal(ev.Data, &r); err != nil {
			return nil, fmt.Errorf("whop: %s data: %w", ev.Type, err)
		}
		s, err := r.snapshot()
		if err != nil {
			return nil, err
		}
		n.Kind, n.Refund = connector.NotifyRefund, &s
	default:
		return nil, nil
	}
	if err := n.Validate(); err != nil { // e.g. an entity with no id (found by fuzzing)
		return nil, fmt.Errorf("whop: %w", err)
	}
	return []connector.Notification{n}, nil
}

// FetchPayment reads a payment by id, or the payments of a checkout
// configuration (best one wins; none means the customer has not paid yet).
func (w *whop) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	switch {
	case ref.ProviderPaymentID != "":
		var p paymentEntity
		if err := w.do(ctx, "GET", "/payments/"+url.PathEscape(ref.ProviderPaymentID), nil, nil, &p, nil); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		return w.withRef(p, ref)
	case ref.ProviderOrderID != "":
		// Whop does not apply the checkout_configuration_id filter on
		// GET /payments (seen in the sandbox): it returns the company's
		// payments. Only a payment whose checkout id or metadata ref is ours
		// may count, so another customer's payment can never mark this paid.
		mine, err := w.paymentsForCheckout(ctx, ref)
		if err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if len(mine) == 0 {
			var cc struct {
				ID   string `json:"id"`
				Plan struct {
					InitialPrice number `json:"initial_price"`
					Currency     string `json:"currency"`
				} `json:"plan"`
			}
			if err := w.do(ctx, "GET", "/checkout_configurations/"+url.PathEscape(ref.ProviderOrderID), nil, nil, &cc, nil); err != nil {
				return connector.PaymentSnapshot{}, err
			}
			cur := strings.ToUpper(cc.Plan.Currency)
			amt, err := minor(cc.Plan.InitialPrice, firstNonEmpty(cur, "USD"))
			if err != nil {
				return connector.PaymentSnapshot{}, err
			}
			return connector.PaymentSnapshot{ProviderRef: ref.ProviderRef, ProviderOrderID: ref.ProviderOrderID,
				Status: connector.PaymentRequiresAction, RawStatus: "no_payment", Amount: amt, Currency: cur}, nil
		}
		best := mine[0]
		for _, p := range mine[1:] {
			if rank(p) > rank(best) {
				best = p
			}
		}
		if best.CheckoutConfig == "" {
			best.CheckoutConfig = ref.ProviderOrderID
		}
		return w.withRef(best, ref)
	}
	return connector.PaymentSnapshot{}, errors.New("whop: PaymentRef needs a payment id or checkout configuration id")
}

func rank(p paymentEntity) int {
	switch mapPaymentStatus(p.Status, p.Substatus) {
	case connector.PaymentSucceeded:
		return 4
	case connector.PaymentProcessing:
		return 3
	case connector.PaymentRequiresAction:
		return 2
	}
	return 1
}

func (w *whop) withRef(p paymentEntity, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	// A payment id may come from outside (the return URL carries one, unsigned).
	// When the caller names a checkout or a reference, only a payment that
	// belongs to it counts; another payment of the company never does.
	if ref.ProviderOrderID != "" || ref.ProviderRef != "" {
		ours := (ref.ProviderOrderID != "" && p.CheckoutConfig == ref.ProviderOrderID) ||
			(ref.ProviderRef != "" && p.Metadata["ref"] == ref.ProviderRef)
		if !ours {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s is not of the checkout or reference asked about", connector.ErrNotFound, p.ID)
		}
	}
	s, err := p.snapshot()
	if s.ProviderRef == "" {
		s.ProviderRef = ref.ProviderRef
	}
	return s, err
}

// Refund refunds a payment (partially if Amount is less than the total).
// The refund provider_ref is sent as an Idempotency-Key. Whop answers with the
// payment, not a refund object, so the refund id is the provider_ref until a
// refund.* webhook or FetchRefund supplies the real one.
func (w *whop) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	if req.ProviderPaymentID == "" || req.Amount <= 0 {
		return connector.RefundSnapshot{}, errors.New("whop: refund needs a payment id and a positive amount")
	}
	price, err := money.FormatDecimal(req.Amount, req.Currency)
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	in := map[string]any{"partial_amount": json.Number(price)}
	var out struct {
		refundEntity
		RefundObj *refundEntity `json:"refund"`
	}
	err = w.do(ctx, "POST", "/payments/"+url.PathEscape(req.ProviderPaymentID)+"/refund", nil, in, &out,
		map[string]string{"Idempotency-Key": req.ProviderRef})
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 422) {
			return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: ae.Message}
		}
		return connector.RefundSnapshot{}, err
	}
	snap := connector.RefundSnapshot{
		ProviderRef: req.ProviderRef, ProviderRefundID: req.ProviderRef, ProviderPaymentID: req.ProviderPaymentID,
		Status: connector.RefundPending, RawStatus: "requested", Amount: req.Amount, Currency: req.Currency,
	}
	if out.RefundObj != nil {
		if s, err := out.RefundObj.snapshot(); err == nil && s.ProviderRefundID != "" {
			s.ProviderRef, s.ProviderPaymentID = req.ProviderRef, req.ProviderPaymentID
			return s, nil
		}
	}
	return snap, nil
}

// FetchRefund reads a Whop refund by id, or derives the state from the payment
// when only the synthetic id (the provider_ref) is known.
func (w *whop) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	if strings.HasPrefix(ref.ProviderRefundID, "rfnd_") {
		var r refundEntity
		if err := w.do(ctx, "GET", "/refunds/"+url.PathEscape(ref.ProviderRefundID), nil, nil, &r, nil); err != nil {
			return connector.RefundSnapshot{}, err
		}
		s, err := r.snapshot()
		s.ProviderRef = ref.ProviderRef
		return s, err
	}
	if ref.ProviderPaymentID == "" {
		return connector.RefundSnapshot{}, errors.New("whop: RefundRef needs a rfnd_ id or a payment id")
	}
	var p paymentEntity
	if err := w.do(ctx, "GET", "/payments/"+url.PathEscape(ref.ProviderPaymentID), nil, nil, &p, nil); err != nil {
		return connector.RefundSnapshot{}, err
	}
	ps, err := p.snapshot()
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	snap := connector.RefundSnapshot{
		ProviderRef: ref.ProviderRef, ProviderRefundID: firstNonEmpty(ref.ProviderRefundID, ref.ProviderRef),
		ProviderPaymentID: p.ID, Status: connector.RefundPending, RawStatus: ps.RawStatus, Amount: ps.AmountRefunded, Currency: ps.Currency,
	}
	if ps.AmountRefunded > 0 {
		snap.Status = connector.RefundSucceeded
	}
	return snap, nil
}

var _ connector.Refunder = (*whop)(nil)

// setRedirect adds the return URL when it is https. Whop rejects any other
// redirect_url, so local http setups rely on the poller and webhooks instead.
func (w *whop) setRedirect(in map[string]any, ref string) {
	if u := w.deps.URLs.ReturnURL(ref); strings.HasPrefix(u, "https://") {
		in["redirect_url"] = u
	}
}

// oneTimeProduct names the product Whop shows at checkout. Without it Whop
// attaches the plan to an unrelated existing product. The external id is
// stable per app and description so repeat payments share one product.
func oneTimeProduct(appID, description string) map[string]any {
	title := firstNonEmptyStr(description, "Payment")
	sum := sha256.Sum256([]byte(appID + "\x00" + title))
	return map[string]any{"title": title, "external_identifier": "paymux-" + hex.EncodeToString(sum[:8])}
}

// maxPaymentPages bounds the scan for a checkout's payments; webhooks carry the
// payment id directly, so this list path is only the poller's fallback.
const maxPaymentPages = 5

// paymentsForCheckout lists recent payments and keeps only those that belong
// to ref: same checkout configuration id, or our provider ref in metadata.
func (w *whop) paymentsForCheckout(ctx context.Context, ref connector.PaymentRef) ([]paymentEntity, error) {
	var mine []paymentEntity
	q := url.Values{"checkout_configuration_id": {ref.ProviderOrderID}, "first": {"100"}}
	for page := 0; page < maxPaymentPages; page++ {
		var list struct {
			Data     []paymentEntity `json:"data"`
			PageInfo struct {
				EndCursor   string `json:"end_cursor"`
				HasNextPage bool   `json:"has_next_page"`
			} `json:"page_info"`
		}
		if err := w.do(ctx, "GET", "/payments", q, nil, &list, nil); err != nil {
			return nil, err
		}
		for _, p := range list.Data {
			if p.CheckoutConfig == ref.ProviderOrderID || (ref.ProviderRef != "" && p.Metadata["ref"] == ref.ProviderRef) {
				mine = append(mine, p)
			}
		}
		if len(mine) > 0 || !list.PageInfo.HasNextPage || list.PageInfo.EndCursor == "" {
			break
		}
		q.Set("after", list.PageInfo.EndCursor)
	}
	return mine, nil
}
