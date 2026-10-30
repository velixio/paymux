package zoho

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// UPI mandates (UPI AutoPay): Paymux triggers every debit (merchant_triggered).
//
// Zoho's flow, from its API documentation (the endpoints exist: GET /mandates
// and GET /mandates/{id} were checked against the live API; the rest has not
// run against a connected account yet, see docs/provider-verification.md):
//
//	registration: POST /customers, then POST /paymentsessions with
//	  type=mandate_enrollment and mandate_details; the widget completes it. The
//	  mandate id is known only after the customer authorises (it is in the
//	  enrollment payment's payment_method.mandate_id and in the widget response).
//	each debit:   POST /mandates/notify (execution date T+1 to T+3), then
//	  POST /paymentsessions with type=mandate_execution, then POST /mandates/execute.
//	revocation:   no API; the customer ends the mandate in their UPI app and
//	  mandate.inactive reports it.
//
// Zoho state that Paymux needs again later is kept in the account's KV: the
// customer of a person, the session of a registration or a debit (so a retried
// call does not create a second one), and the notice sent for a debit.

const (
	// upiUnattendedCap is the largest UPI AutoPay debit that needs no
	// authentication of the customer (RBI), in paise: Rs 15,000.
	upiUnattendedCap int64 = 1_500_000

	kvTTL = 30 * 24 * time.Hour
)

var ist = time.FixedZone("IST", 5*3600+1800)

type mandateEntity struct {
	MandateID  string `json:"mandate_id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
	// Amount is "amount" (the mandate object and the mandate events); a variable
	// mandate's amount is its maximum. MaxAmount is read when Zoho sends one.
	Amount    number         `json:"amount"`
	MaxAmount number         `json:"max_amount"`
	Currency  string         `json:"currency"`
	Reference string         `json:"reference_number"`
	EndDate   int64          `json:"end_date"` // unix seconds
	Method    methodField    `json:"payment_method"`
	Payment   *paymentEntity `json:"payment"`
}

func mapMandateStatus(raw string) string {
	switch strings.ToLower(raw) {
	case "active", "resumed", "authorized", "authorised", "live":
		return "active"
	case "inactive", "revoked", "cancelled", "canceled":
		return "revoked"
	case "expired", "completed":
		return "expired"
	case "failed", "rejected", "declined":
		return "failed"
	}
	return "pending" // "created", "paused", "pending", "initiated" and anything new
}

func (m mandateEntity) snapshot() (connector.MandateSnapshot, error) {
	a := m.MaxAmount
	if a == "" {
		a = m.Amount
	}
	amt, err := a.minor()
	if err != nil {
		return connector.MandateSnapshot{}, fmt.Errorf("zoho: mandate amount: %w", err)
	}
	snap := connector.MandateSnapshot{ProviderMandateID: m.MandateID, Status: mapMandateStatus(m.Status), RawStatus: m.Status,
		MaxAmount: amt, Currency: firstNonEmpty(m.Currency, "INR"), Method: "upi"}
	if m.EndDate > 0 {
		snap.ValidUntil = m.EndDate * 1000
	}
	if m.Payment != nil {
		ps, err := m.Payment.snapshot("")
		if err != nil {
			return connector.MandateSnapshot{}, err
		}
		snap.SetupPayment = &ps
	}
	return snap, nil
}

// ---- KV helpers ----

func (z *zoho) kvGet(ctx context.Context, key string) string {
	b, ok, err := z.deps.KV.Get(ctx, key)
	if err != nil || !ok {
		return ""
	}
	return string(b)
}

func (z *zoho) kvSet(ctx context.Context, key, val string) {
	_ = z.deps.KV.Set(ctx, key, []byte(val), kvTTL)
}

var nonDigit = regexp.MustCompile(`\D`)

// customerFor returns the Zoho customer of a person, creating it the first time
// (Zoho documents no lookup by e-mail, so the id is remembered).
func (z *zoho) customerFor(ctx context.Context, c connector.Customer) (string, error) {
	phone := nonDigit.ReplaceAllString(c.Phone, "")
	if len(phone) > 10 {
		phone = phone[len(phone)-10:]
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	sum := sha256.Sum256([]byte(email + "|" + phone + "|" + c.Name))
	key := "zoho:cust:" + hex.EncodeToString(sum[:8])
	if id := z.kvGet(ctx, key); id != "" {
		return id, nil
	}
	body := map[string]any{"name": firstNonEmpty(c.Name, "Customer"), "email": firstNonEmpty(email, "customer@example.com"), "phone_country_code": "IN"}
	if phone != "" {
		body["phone"] = phone
	}
	var out struct {
		Customer struct {
			ID string `json:"customer_id"`
		} `json:"customer"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/customers", nil, body, &out); err != nil {
		return "", err
	}
	if out.Customer.ID == "" {
		return "", errors.New("zoho: customer response has no customer_id")
	}
	z.kvSet(ctx, key, out.Customer.ID)
	return out.Customer.ID, nil
}

// zohoDate is Zoho's dd-mm-yyyy, in IST.
func zohoDate(t time.Time) string { return t.In(ist).Format("02-01-2006") }

// invoiceNumber is a reference as an invoice_number (at most 50 characters).
func invoiceNumber(ref string) string {
	if len(ref) <= 50 {
		return ref
	}
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:32]
}

// widgetOptions are the options of the checkout widget (or the hosted page) for a session.
func (z *zoho) sessionAction(sid, amount, description, returnURL string) connector.NextAction {
	if z.cfg.WidgetAPIKey != "" {
		opts := map[string]any{
			"account_id": z.cfg.AccountID, "api_key": z.cfg.WidgetAPIKey, "domain": strings.ToUpper(z.cfg.dc()),
			"payments_session_id": sid, "amount": amount, "currency": "INR", "description": description, "return_url": returnURL,
		}
		if z.cfg.Sandbox {
			opts["is_test_mode"] = true
		}
		return connector.NextAction{Kind: connector.ActionSDK, ScriptURL: widgetScript, Options: opts}
	}
	sep := "?"
	if strings.Contains(z.cfg.HostedCheckoutURL, "?") {
		sep = "&"
	}
	return connector.NextAction{Kind: connector.ActionRedirect, URL: z.cfg.HostedCheckoutURL + sep + "payments_session_id=" + url.QueryEscape(sid)}
}

// CreateMandateCheckout creates the customer and a mandate enrollment session,
// and returns the widget (sdk) or hosted-page (redirect) action that completes it.
// The registration transaction debits req.Amount; the mandate's limit for later
// debits is req.MaxAmount. No provider id is returned: the mandate's id exists
// only once the customer has authorised, and is then found from the reference.
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
	first := req.Amount
	if first <= 0 {
		first = 100 // Info.MandateSetupAmount: the verification debit
	}
	// Zoho: a future-dated mandate is authorised with exactly Rs 1; a same-day
	// one debits its first amount and needs at least Rs 6. Nothing in between.
	if first != 100 && first < 600 {
		return connector.CheckoutResult{}, fmt.Errorf("zoho: a mandate registration debits Rs 1 (future-dated) or at least Rs 6 (same-day), not %d paise", first)
	}
	max := req.MaxAmount
	if max < first {
		max = first
	}
	firstStr, err := money.FormatDecimal(first, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	maxStr, err := money.FormatDecimal(max, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	cust, err := z.customerFor(ctx, req.Customer)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	desc := "Recurring payment authorisation"
	details := map[string]any{
		"payment_method_type": "upi", "frequency": "as_presented", "description": desc,
		"amount_rule": "variable", "max_amount": json.Number(maxStr),
	}
	// A same-day mandate needs at least Rs 6; the Rs 1 verification debit is for a
	// future-dated one.
	if first < 600 {
		details["start_date"] = zohoDate(z.deps.Now().Add(24 * time.Hour))
	}
	if req.ValidUntil > 0 {
		details["end_date"] = zohoDate(time.UnixMilli(req.ValidUntil))
	}
	in := map[string]any{
		"amount": json.Number(firstStr), "currency": "INR", "customer_id": cust, "type": "mandate_enrollment",
		"description": desc, "invoice_number": invoiceNumber(req.ProviderRef), "max_retry_count": 3, "mandate_details": details,
		"meta_data": []metaItem{{Key: metaRef, Value: req.ProviderRef}, {Key: "mdt", Value: req.MandateID}},
	}
	var out struct {
		Session struct {
			ID string `json:"payments_session_id"`
		} `json:"payments_session"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/paymentsessions", nil, in, &out); err != nil {
		return connector.CheckoutResult{}, err
	}
	if out.Session.ID == "" {
		return connector.CheckoutResult{}, errors.New("zoho: mandate session response has no id")
	}
	z.kvSet(ctx, "zoho:msess:"+req.ProviderRef, out.Session.ID)
	return connector.CheckoutResult{NextAction: z.sessionAction(out.Session.ID, firstStr, desc, z.deps.URLs.ReturnURL(req.ProviderRef))}, nil
}

// FetchMandate reads a mandate. The id is Zoho's mandate id, or, until the
// customer has authorised, Paymux's reference of the registration, which is
// resolved through the enrollment session created for it.
func (z *zoho) FetchMandate(ctx context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	id := ref.ProviderMandateID
	if id == "" {
		return connector.MandateSnapshot{}, errors.New("zoho: MandateRef has no id")
	}
	if sid := z.kvGet(ctx, "zoho:msess:"+id); sid != "" {
		return z.fetchMandateOfSession(ctx, id, sid)
	}
	if !nonDigit.MatchString(id) { // all digits: a Zoho id
		return z.fetchMandateByID(ctx, id)
	}
	return connector.MandateSnapshot{}, fmt.Errorf("%w: no mandate registration for %q", connector.ErrNotFound, id)
}

func (z *zoho) fetchMandateByID(ctx context.Context, id string) (connector.MandateSnapshot, error) {
	var out struct {
		Mandate mandateEntity `json:"mandate"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/mandates/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return connector.MandateSnapshot{}, err
	}
	if out.Mandate.MandateID == "" {
		out.Mandate.MandateID = id
	}
	return out.Mandate.snapshot()
}

// fetchMandateOfSession finds the mandate a registration produced: the enrollment
// payment names it in its payment_method.
func (z *zoho) fetchMandateOfSession(ctx context.Context, ref, sid string) (connector.MandateSnapshot, error) {
	pay, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: sid, ProviderRef: ""})
	if err != nil {
		return connector.MandateSnapshot{}, err
	}
	if pay.ProviderPaymentID == "" {
		// Not authorised yet (or the session expired unused).
		st := "pending"
		if pay.Status == connector.PaymentExpired {
			st = "failed"
		}
		return connector.MandateSnapshot{Status: st, RawStatus: pay.RawStatus, MaxAmount: 0, Currency: "INR", Method: "upi"}, nil
	}
	var raw struct {
		Payment paymentEntity `json:"payment"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/payments/"+url.PathEscape(pay.ProviderPaymentID), nil, nil, &raw); err != nil {
		return connector.MandateSnapshot{}, err
	}
	pay.ProviderRef = ref
	mid := raw.Payment.Method.MandateID
	if mid == "" || pay.Status == connector.PaymentFailed || pay.Status == connector.PaymentCanceled || pay.Status == connector.PaymentExpired {
		st := "pending"
		if pay.Status == connector.PaymentFailed || pay.Status == connector.PaymentCanceled || pay.Status == connector.PaymentExpired {
			st = "failed"
		}
		return connector.MandateSnapshot{Status: st, RawStatus: pay.RawStatus, Currency: "INR", Method: "upi"}, nil
	}
	snap, err := z.fetchMandateByID(ctx, mid)
	if err != nil {
		return connector.MandateSnapshot{}, err
	}
	if pay.Status == connector.PaymentSucceeded {
		snap.SetupPayment = &pay
	}
	return snap, nil
}

// NotifyPreDebit sends the notice Zoho wants before a debit (POST /mandates/notify):
// the execution date must be T+1 to T+3 (IST). For a debit further away there is
// nothing to send yet: ErrUnsupported tells the caller no notice is due, and the
// execute call notifies the customer by itself (and debits 24 hours later).
func (z *zoho) NotifyPreDebit(ctx context.Context, req connector.PreDebitRequest) error {
	if req.Currency != "INR" {
		return fmt.Errorf("zoho: currency %q unsupported (INR only)", req.Currency)
	}
	if z.kvGet(ctx, "zoho:notif:"+req.ProviderRef) != "" {
		return nil // already sent: the job is being retried
	}
	today := z.deps.Now().In(ist)
	day := time.UnixMilli(req.DebitDate).In(ist)
	d0 := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, ist)
	d1 := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, ist)
	if diff := int(d1.Sub(d0).Hours() / 24); diff < 1 || diff > 3 {
		return fmt.Errorf("%w: Zoho takes a notice for an execution date of tomorrow to three days ahead, not %s", connector.ErrUnsupported, zohoDate(day))
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return err
	}
	in := map[string]any{
		"mandate_id": idNumber(req.Mandate.ProviderMandateID), "amount": json.Number(amt), "execution_date": zohoDate(day),
		"description": "Upcoming recurring payment", "invoice_number": invoiceNumber(req.ProviderRef),
	}
	var out struct {
		Notification struct {
			ID string `json:"mandate_notification_id"`
		} `json:"mandate_notification"`
	}
	if err := z.api.do(ctx, "POST", "/api/v1/mandates/notify", nil, in, &out); err != nil {
		return err
	}
	z.kvSet(ctx, "zoho:notif:"+req.ProviderRef, firstNonEmpty(out.Notification.ID, "sent"))
	return nil
}

// idNumber sends a numeric Zoho id as a JSON number (the API documents its ids as long).
func idNumber(id string) any {
	if !nonDigit.MatchString(id) && id != "" {
		return json.Number(id)
	}
	return id
}

// inNPCIClosedWindow reports whether UPI AutoPay executions are closed:
// 10:00 to 13:00 and 17:00 to 21:30 IST (Zoho's documentation, per NPCI).
func inNPCIClosedWindow(t time.Time) bool {
	t = t.In(ist)
	m := t.Hour()*60 + t.Minute()
	return (m >= 10*60 && m < 13*60) || (m >= 17*60 && m < 21*60+30)
}

// ChargeMandate debits the mandate: an execution session, then POST
// /mandates/execute. ProviderRef is the session's invoice_number and the
// payment's reference_number; the session of a reference is remembered, so a
// repeated charge returns the first debit and never debits twice. The result is
// "initiated": the payment's outcome comes by webhook or FetchPayment.
func (z *zoho) ChargeMandate(ctx context.Context, req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	if req.Currency != "INR" {
		return connector.PaymentSnapshot{}, fmt.Errorf("zoho: currency %q unsupported (INR only)", req.Currency)
	}
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.PaymentSnapshot{}, errors.New("zoho: charge needs a positive amount and a provider_ref")
	}
	// A debit already made for this reference: return it.
	if sid := z.kvGet(ctx, "zoho:execd:"+req.ProviderRef); sid != "" {
		snap, err := z.FetchPayment(ctx, connector.PaymentRef{ProviderOrderID: sid, ProviderRef: req.ProviderRef})
		if err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if snap.Status == connector.PaymentRequiresAction {
			snap.Status = connector.PaymentProcessing // initiated, not listed on the session yet
		}
		return snap, nil
	}
	if req.Amount > upiUnattendedCap {
		return connector.PaymentSnapshot{}, connector.ErrMandateLimit // RBI: above Rs 15,000 each debit needs the customer's authentication
	}
	if inNPCIClosedWindow(z.deps.Now()) {
		return connector.PaymentSnapshot{}, fmt.Errorf("%w: NPCI does not run UPI AutoPay executions between 10:00 and 13:00 or 17:00 and 21:30 IST; the debit waits", connector.ErrTemporary)
	}
	md, err := z.fetchMandateEntity(ctx, req.Mandate.ProviderMandateID)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	if mapMandateStatus(md.Status) != "active" {
		return connector.PaymentSnapshot{ProviderRef: req.ProviderRef, Status: connector.PaymentFailed, RawStatus: md.Status, Amount: req.Amount, Currency: "INR",
			ErrorCode: "mandate_inactive", ErrorMessage: "the mandate is " + md.Status}, nil
	}
	if ms, err := md.snapshot(); err == nil && ms.MaxAmount > 0 && req.Amount > ms.MaxAmount {
		return connector.PaymentSnapshot{}, connector.ErrMandateLimit
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	sid := z.kvGet(ctx, "zoho:exec:"+req.ProviderRef)
	if sid == "" {
		in := map[string]any{
			"amount": json.Number(amt), "currency": "INR", "customer_id": md.CustomerID, "type": "mandate_execution",
			"description": "Recurring payment", "invoice_number": invoiceNumber(req.ProviderRef), "max_retry_count": 1,
			"meta_data": []metaItem{{Key: metaRef, Value: req.ProviderRef}},
		}
		var out struct {
			Session struct {
				ID string `json:"payments_session_id"`
			} `json:"payments_session"`
		}
		if err := z.api.do(ctx, "POST", "/api/v1/paymentsessions", nil, in, &out); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if sid = out.Session.ID; sid == "" {
			return connector.PaymentSnapshot{}, errors.New("zoho: execution session response has no id")
		}
		z.kvSet(ctx, "zoho:exec:"+req.ProviderRef, sid)
	}
	ex := map[string]any{
		"customer_id": idNumber(md.CustomerID), "mandate_id": idNumber(req.Mandate.ProviderMandateID), "payments_session_id": idNumber(sid),
		"invoice_number": invoiceNumber(req.ProviderRef), "amount": json.Number(amt), "reference_number": req.ProviderRef, "description": "Recurring payment",
	}
	if n := z.kvGet(ctx, "zoho:notif:"+req.ProviderRef); n != "" && n != "sent" && !nonDigit.MatchString(n) {
		ex["mandate_notification_id"] = json.Number(n)
	}
	var out struct {
		Payment paymentEntity `json:"payment"`
	}
	err = z.api.do(ctx, "POST", "/api/v1/mandates/execute", nil, ex, &out)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 400 {
		low := strings.ToLower(ae.Message)
		if strings.Contains(low, "limit") || strings.Contains(low, "exceed") {
			return connector.PaymentSnapshot{}, connector.ErrMandateLimit
		}
		// The debit was refused (mandate inactive, ...): a failed payment.
		return connector.PaymentSnapshot{ProviderRef: req.ProviderRef, ProviderOrderID: sid, Status: connector.PaymentFailed, RawStatus: "rejected",
			Amount: req.Amount, Currency: "INR", ErrorCode: "mandate_rejected", ErrorMessage: ae.Message}, nil
	}
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	z.kvSet(ctx, "zoho:execd:"+req.ProviderRef, sid)
	s, err := out.Payment.snapshot(sid)
	s.ProviderRef, s.ProviderOrderID = req.ProviderRef, sid
	if s.Amount == 0 {
		s.Amount, s.Currency = req.Amount, "INR"
	}
	return s, err
}

func (z *zoho) fetchMandateEntity(ctx context.Context, id string) (mandateEntity, error) {
	var out struct {
		Mandate mandateEntity `json:"mandate"`
	}
	if err := z.api.do(ctx, "GET", "/api/v1/mandates/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return mandateEntity{}, err
	}
	return out.Mandate, nil
}

// RevokeMandate is not supported: Zoho has no API to end a mandate. The customer
// ends it in their UPI app, and mandate.inactive reports it.
func (z *zoho) RevokeMandate(context.Context, connector.MandateRef) error {
	return fmt.Errorf("%w: Zoho has no API to revoke a mandate; the customer cancels it in their UPI app", connector.ErrUnsupported)
}

var _ connector.MandateManager = (*zoho)(nil)
