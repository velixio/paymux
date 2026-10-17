package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// UPI mandates (UPI AutoPay): Paymux triggers every debit (merchant_triggered).
//
// TODO(spec): re-verify against the Zoho Payments docs at sandbox time. The
// endpoints below (/api/v1/mandates, .../charges, .../revoke), their request
// fields and the mandate statuses are modelled on the payment-session and
// refund APIs and are NOT confirmed. They are all in this file.

type mandateEntity struct {
	MandateID string `json:"mandate_id"`
	SessionID string `json:"payments_session_id"`
	Status    string `json:"status"`
	Amount    number `json:"max_amount"`
	Currency  string `json:"currency"`
	Reference string `json:"reference_number"`
	EndDate   string `json:"end_date"`
	// Payment is the registration transaction, when Zoho reports it.
	Payment *paymentEntity `json:"payment"`
}

func mapMandateStatus(raw string) string {
	switch strings.ToLower(raw) {
	case "active", "authorized", "authorised", "live":
		return "active"
	case "revoked", "cancelled", "canceled", "paused_by_customer":
		return "revoked"
	case "expired", "completed":
		return "expired"
	case "failed", "rejected", "declined":
		return "failed"
	}
	return "pending" // "created", "pending", "initiated" and anything new
}

func (m mandateEntity) snapshot() (connector.MandateSnapshot, error) {
	amt, err := m.Amount.minor()
	if err != nil {
		return connector.MandateSnapshot{}, fmt.Errorf("zoho: mandate amount: %w", err)
	}
	snap := connector.MandateSnapshot{ProviderMandateID: m.MandateID, Status: mapMandateStatus(m.Status), RawStatus: m.Status,
		MaxAmount: amt, Currency: firstNonEmpty(m.Currency, "INR"), Method: "upi"}
	if m.Payment != nil {
		ps, err := m.Payment.snapshot("")
		if err != nil {
			return connector.MandateSnapshot{}, err
		}
		if ps.ProviderRef == "" {
			ps.ProviderRef = m.Reference
		}
		snap.SetupPayment = &ps
	}
	return snap, nil
}

// CreateMandateCheckout registers a UPI mandate and returns the checkout for
// its authorisation: the widget (sdk) or the hosted page (redirect), exactly
// like a payment session.
func (z *zoho) CreateMandateCheckout(ctx context.Context, req connector.MandateRequest) (connector.CheckoutResult, error) {
	if req.Currency != "INR" {
		return connector.CheckoutResult{}, fmt.Errorf("zoho: currency %q unsupported (INR only)", req.Currency)
	}
	if req.Method != "upi" && req.Method != "" {
		return connector.CheckoutResult{}, fmt.Errorf("zoho: only UPI mandates are supported, not %q", req.Method)
	}
	if req.ProviderRef == "" || req.MaxAmount <= 0 {
		return connector.CheckoutResult{}, errors.New("zoho: provider_ref and a positive max amount are required")
	}
	maxStr, err := money.FormatDecimal(req.MaxAmount, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	first := req.Amount
	if first <= 0 {
		first = 100 // Info.MandateSetupAmount: the verification debit
	}
	firstStr, err := money.FormatDecimal(first, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	in := map[string]any{
		// amount is the registration's own (AFA) debit; max_amount caps later ones.
		"amount": json.Number(firstStr), "max_amount": json.Number(maxStr), "currency": "INR", "frequency": "as_presented",
		"reference_number": req.ProviderRef, "return_url": z.deps.URLs.ReturnURL(req.ProviderRef),
		"customer":  map[string]string{"name": req.Customer.Name, "email": req.Customer.Email, "phone": req.Customer.Phone},
		"meta_data": []map[string]string{{"key": "mdt", "value": req.MandateID}},
	}
	if req.ValidUntil > 0 {
		in["end_date"] = istDay(req.ValidUntil)
	}
	var out struct {
		Mandate mandateEntity `json:"mandate"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/mandates", nil, in, &out); err != nil {
		return connector.CheckoutResult{}, err
	}
	m := out.Mandate
	if m.MandateID == "" || m.SessionID == "" {
		return connector.CheckoutResult{}, errors.New("zoho: mandate response lacks mandate_id or payments_session_id")
	}
	res := connector.CheckoutResult{ProviderOrderID: m.MandateID}
	if z.cfg.WidgetAPIKey != "" {
		res.NextAction = connector.NextAction{Kind: connector.ActionSDK, ScriptURL: widgetScript, Options: map[string]any{
			"account_id": z.cfg.AccountID, "api_key": z.cfg.WidgetAPIKey, "domain": strings.ToUpper(z.cfg.dc()),
			"payments_session_id": m.SessionID, "amount": maxStr, "currency": "INR",
			"return_url": z.deps.URLs.ReturnURL(req.ProviderRef),
		}}
	} else {
		sep := "?"
		if strings.Contains(z.cfg.HostedCheckoutURL, "?") {
			sep = "&"
		}
		res.NextAction = connector.NextAction{Kind: connector.ActionRedirect,
			URL: z.cfg.HostedCheckoutURL + sep + "payments_session_id=" + url.QueryEscape(m.SessionID)}
	}
	return res, nil
}

// FetchMandate reads a mandate by its Zoho id.
func (z *zoho) FetchMandate(ctx context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	if ref.ProviderMandateID == "" {
		return connector.MandateSnapshot{}, errors.New("zoho: MandateRef has no id")
	}
	var out struct {
		Mandate mandateEntity `json:"mandate"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/mandates/"+url.PathEscape(ref.ProviderMandateID), nil, nil, &out); err != nil {
		return connector.MandateSnapshot{}, err
	}
	return out.Mandate.snapshot()
}

// NotifyPreDebit is not supported: the UPI app (NPCI) tells the customer
// before every AutoPay debit, and Zoho has no notice API. Callers treat
// ErrUnsupported as "no notice needed".
func (z *zoho) NotifyPreDebit(context.Context, connector.PreDebitRequest) error {
	return connector.ErrUnsupported
}

// ChargeMandate presents a debit against the mandate. ProviderRef is sent as
// reference_number; a repeat with the same reference returns the first debit.
func (z *zoho) ChargeMandate(ctx context.Context, req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	if req.Currency != "INR" {
		return connector.PaymentSnapshot{}, fmt.Errorf("zoho: currency %q unsupported (INR only)", req.Currency)
	}
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.PaymentSnapshot{}, errors.New("zoho: charge needs a positive amount and a provider_ref")
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	in := map[string]any{"amount": json.Number(amt), "currency": "INR", "reference_number": req.ProviderRef}
	var out struct {
		Payment paymentEntity `json:"payment"`
	}
	err = z.api.do(ctx, "POST", "/api/v1/mandates/"+url.PathEscape(req.Mandate.ProviderMandateID)+"/charges", nil, in, &out)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 400 {
		low := strings.ToLower(ae.Message)
		if strings.Contains(low, "limit") || strings.Contains(low, "exceed") {
			return connector.PaymentSnapshot{}, connector.ErrMandateLimit
		}
		// The debit was refused (mandate inactive, ...): a failed payment.
		return connector.PaymentSnapshot{ProviderRef: req.ProviderRef, Status: connector.PaymentFailed, RawStatus: "rejected",
			Amount: req.Amount, Currency: "INR", ErrorCode: "mandate_rejected", ErrorMessage: ae.Message}, nil
	}
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	s, err := out.Payment.snapshot("")
	if s.ProviderRef == "" {
		s.ProviderRef = req.ProviderRef
	}
	if s.Amount == 0 {
		s.Amount, s.Currency = req.Amount, "INR"
	}
	return s, err
}

// RevokeMandate cancels the mandate at Zoho.
func (z *zoho) RevokeMandate(ctx context.Context, ref connector.MandateRef) error {
	err := z.api.do(ctx, "POST", "/api/v1/mandates/"+url.PathEscape(ref.ProviderMandateID)+"/revoke", nil, map[string]any{}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 400 && strings.Contains(strings.ToLower(ae.Message), "already") {
		return nil
	}
	return err
}

func istDay(ms int64) string {
	return timeFromMS(ms).Format("2006-01-02")
}

var _ connector.MandateManager = (*zoho)(nil)
