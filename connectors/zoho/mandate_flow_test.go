package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// Zoho's real mandate flow (its API documentation): a customer, an enrollment
// payment session (type mandate_enrollment) that the widget completes, then for
// every debit POST /mandates/notify, an execution session (type mandate_execution)
// and POST /mandates/execute. There is no revoke call. The earlier connector
// modelled other endpoints (POST /mandates answers 405).

// 2025-10-09 14:23:20 IST: a time at which NPCI allows executions.
const mandateNow = int64(1760000000)

func (f *rfake) connAt(t testing.TB, cfg json.RawMessage, now time.Time) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(cfg, connectortest.NewDeps(srv, now))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mandateFake() *rfake {
	f := newRFake()
	f.on("POST /api/v1/customers", 201, []byte(`{"code":0,"message":"Customer created","customer":{"customer_id":"28000000000400001","name":"Asha","email":"asha@example.com","phone":"9876543210","dialing_code":"+91"}}`))
	f.on("POST /api/v1/paymentsessions", 201, []byte(`{"code":0,"message":"success","payments_session":{"payments_session_id":"28000000000500001","currency":"INR","amount":"499.00","created_time":1760000000,"max_retry_count":3,"meta_data":[]}}`))
	return f
}

func mandateReq() connector.MandateRequest {
	return connector.MandateRequest{MandateID: "mdt_01", ProviderRef: "mdt_ref_01", Amount: 49900, MaxAmount: 149900, Currency: "INR", Method: "upi",
		ValidUntil: (mandateNow + 365*86400) * 1000, Customer: connector.Customer{Name: "Asha", Email: "asha@example.com", Phone: "+91 98765 43210"}}
}

func TestMandateEnrollmentSession(t *testing.T) {
	f := mandateFake()
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	res, err := c.CreateMandateCheckout(context.Background(), mandateReq())
	if err != nil {
		t.Fatal(err)
	}
	connectortest.CheckAllowed(t, c, res.NextAction)
	var cust map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/customers"], &cust)
	if cust["name"] != "Asha" || cust["email"] != "asha@example.com" || cust["phone"] != "9876543210" || cust["phone_country_code"] != "IN" {
		t.Errorf("customer body = %v", cust)
	}
	var s struct {
		Amount     json.Number `json:"amount"`
		Currency   string      `json:"currency"`
		CustomerID string      `json:"customer_id"`
		Type       string      `json:"type"`
		Invoice    string      `json:"invoice_number"`
		Desc       string      `json:"description"`
		Mandate    struct {
			Method string      `json:"payment_method_type"`
			Freq   string      `json:"frequency"`
			Rule   string      `json:"amount_rule"`
			Max    json.Number `json:"max_amount"`
			Start  string      `json:"start_date"`
			End    string      `json:"end_date"`
			Desc   string      `json:"description"`
		} `json:"mandate_details"`
		Meta []metaItem `json:"meta_data"`
	}
	_ = json.Unmarshal(f.bodies["POST /api/v1/paymentsessions"], &s)
	if s.Amount.String() != "499.00" || s.Currency != "INR" || s.CustomerID != "28000000000400001" || s.Type != "mandate_enrollment" || s.Invoice != "mdt_ref_01" || s.Desc == "" ||
		s.Mandate.Method != "upi" || s.Mandate.Freq != "as_presented" || s.Mandate.Rule != "variable" || s.Mandate.Max.String() != "1499.00" || s.Mandate.Desc == "" ||
		s.Mandate.Start != "" || s.Mandate.End != "09-10-2026" {
		t.Errorf("session body = %s", f.bodies["POST /api/v1/paymentsessions"])
	}
	if len(s.Meta) < 1 || s.Meta[0].Key != "paymux_ref" || s.Meta[0].Value != "mdt_ref_01" || len(s.Meta) > 5 {
		t.Errorf("meta_data = %v", s.Meta)
	}
	// The mandate id exists only once the customer has authorised: the core must
	// keep asking by Paymux's own reference, so no provider id is handed back.
	na := res.NextAction
	if res.ProviderOrderID != "" || na.Kind != connector.ActionSDK || na.Options["payments_session_id"] != "28000000000500001" || na.Options["amount"] != "499.00" || na.Options["description"] == "" {
		t.Errorf("%+v", res)
	}
	// The customer is created once for the same person.
	if _, err := c.CreateMandateCheckout(context.Background(), mandateReq()); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, call := range f.calls {
		if call == "POST /api/v1/customers" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("customers created = %d", n)
	}
}

// The verification amount (Rs 1) is allowed only for a future-dated mandate; a
// same-day one needs at least Rs 6.
func TestMandateEnrollmentWithTheSetupAmountIsFutureDated(t *testing.T) {
	f := mandateFake()
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	r := mandateReq()
	r.Amount = 100
	if _, err := c.CreateMandateCheckout(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var s struct {
		Mandate struct {
			Start string `json:"start_date"`
		} `json:"mandate_details"`
	}
	_ = json.Unmarshal(f.bodies["POST /api/v1/paymentsessions"], &s)
	if s.Mandate.Start != "10-10-2025" {
		t.Errorf("start_date = %q (tomorrow, IST)", s.Mandate.Start)
	}
}

func TestMandateEnrollmentRefusals(t *testing.T) {
	f := mandateFake()
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	for name, mut := range map[string]func(*connector.MandateRequest){
		"usd":    func(r *connector.MandateRequest) { r.Currency = "USD" },
		"card":   func(r *connector.MandateRequest) { r.Method = "card" },
		"no ref": func(r *connector.MandateRequest) { r.ProviderRef = "" },
		"no max": func(r *connector.MandateRequest) { r.MaxAmount = 0 },
	} {
		r := mandateReq()
		mut(&r)
		if _, err := c.CreateMandateCheckout(context.Background(), r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("requests for refused mandates: %v", f.calls)
	}
}

// After the customer authorises, the mandate id is in the enrollment payment's
// payment_method (and in the widget response); it is found from Paymux's reference
// through the session created for it.
func TestFetchMandateFromThePaymuxReference(t *testing.T) {
	f := mandateFake()
	f.on("GET /api/v1/paymentsessions/28000000000500001", 200, []byte(`{"code":0,"payments_session":{"payments_session_id":"28000000000500001","status":"succeeded","amount":"499.00","currency":"INR","payments":[{"payment_id":"28000000000600001","status":"succeeded","created_time":1760000100}]}}`))
	f.on("GET /api/v1/payments/28000000000600001", 200, []byte(`{"code":0,"payment":{"payment_id":"28000000000600001","payments_session_id":"28000000000500001","amount":"499.00","currency":"INR","status":"succeeded","payment_method":{"type":"upi","mandate_id":"28000000000700001","upi":{"upi_id":"asha@bank"}}}}`))
	f.on("GET /api/v1/mandates/28000000000700001", 200, []byte(`{"code":0,"message":"success","mandate":{"mandate_id":"28000000000700001","customer_id":"28000000000400001","amount":"1499.00","currency":"INR","amount_rule":"variable","frequency":"as_presented","status":"active","end_date":1791536000,"payment_method":{"type":"upi","upi":{"upi_id":"asha@bank"}}}}`))
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	if _, err := c.CreateMandateCheckout(context.Background(), mandateReq()); err != nil {
		t.Fatal(err)
	}
	s, err := c.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "mdt_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if s.ProviderMandateID != "28000000000700001" || s.Status != "active" || s.MaxAmount != 149900 || s.Method != "upi" || s.ValidUntil != 1791536000000 {
		t.Errorf("%+v", s)
	}
	if sp := s.SetupPayment; sp == nil || sp.Status != connector.PaymentSucceeded || sp.Amount != 49900 || sp.ProviderRef != "mdt_ref_01" || sp.ProviderPaymentID != "28000000000600001" {
		t.Errorf("setup payment = %+v", s.SetupPayment)
	}
	// By Zoho's own id afterwards.
	g, err := c.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "28000000000700001"})
	if err != nil || g.Status != "active" || g.ProviderMandateID != "28000000000700001" {
		t.Errorf("%+v %v", g, err)
	}
}

func TestFetchMandateBeforeAndAfterAFailedAuthorisation(t *testing.T) {
	f := mandateFake()
	f.on("GET /api/v1/paymentsessions/28000000000500001", 200, []byte(`{"code":0,"payments_session":{"payments_session_id":"28000000000500001","status":"created","amount":"499.00","currency":"INR"}}`))
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	if _, err := c.CreateMandateCheckout(context.Background(), mandateReq()); err != nil {
		t.Fatal(err)
	}
	s, err := c.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "mdt_ref_01"})
	if err != nil || s.Status != "pending" || s.ProviderMandateID != "" {
		t.Fatalf("not authorised yet: %+v %v", s, err)
	}
	f.on("GET /api/v1/paymentsessions/28000000000500001", 200, []byte(`{"code":0,"payments_session":{"payments_session_id":"28000000000500001","status":"failed","payments":[{"payment_id":"28000000000600009","status":"failed","created_time":1760000100}]}}`))
	f.on("GET /api/v1/payments/28000000000600009", 200, []byte(`{"code":0,"payment":{"payment_id":"28000000000600009","payments_session_id":"28000000000500001","amount":"499.00","currency":"INR","status":"failed","failure_reason":"declined"}}`))
	s, err = c.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "mdt_ref_01"})
	if err != nil || s.Status != "failed" {
		t.Fatalf("the authorisation failed: %+v %v", s, err)
	}
	// A reference nobody created and a mandate Zoho does not know.
	f.on("GET /api/v1/mandates/mdt_unknown", 400, []byte(`{"code":"resource_does_not_exist","message":"Mandate Reference does not exist."}`))
	if _, err := c.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "mdt_unknown"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestMandateStatusMapping(t *testing.T) {
	for raw, want := range map[string]string{"active": "active", "resumed": "active", "paused": "pending", "inactive": "revoked", "created": "pending", "failed": "failed", "expired": "expired", "revoked": "revoked"} {
		if got := mapMandateStatus(raw); got != want {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
}

// ---- debits ----

func TestNotifyPreDebit(t *testing.T) {
	f := mandateFake()
	f.on("POST /api/v1/mandates/notify", 201, []byte(`{"code":0,"message":"Mandate notification sent successfully","mandate_notification":{"mandate_id":"28000000000700001","amount":"499.00","mandate_notification_id":"28000000000800001","notification_status":"succeeded","description":"d","invoice_number":"att_1"}}`))
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	req := connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "28000000000700001"}, ProviderRef: "att_1", Amount: 49900, Currency: "INR", DebitDate: (mandateNow + 48*3600) * 1000}
	if err := c.NotifyPreDebit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/mandates/notify"], &b)
	if b["execution_date"] != "11-10-2025" || b["invoice_number"] != "att_1" || b["description"] == "" || b["amount"] != 499.0 || b["mandate_id"] != float64(28000000000700001) {
		t.Errorf("body = %v", b)
	}
	// A repeated notice (the job is retried) is not sent twice.
	if err := c.NotifyPreDebit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, call := range f.calls {
		if call == "POST /api/v1/mandates/notify" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("notices sent = %d", n)
	}
	// Zoho takes a notice for T+1 to T+3 only; for a later debit the execute call
	// notifies by itself, so there is nothing to send now.
	far := req
	far.ProviderRef, far.DebitDate = "att_far", (mandateNow+10*86400)*1000
	if err := c.NotifyPreDebit(context.Background(), far); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("a debit more than 3 days away: %v", err)
	}
}

func executeFake() *rfake {
	f := mandateFake()
	f.on("GET /api/v1/mandates/28000000000700001", 200, []byte(`{"code":0,"mandate":{"mandate_id":"28000000000700001","customer_id":"28000000000400001","amount":"1499.00","currency":"INR","status":"active","payment_method":{"type":"upi"}}}`))
	f.on("POST /api/v1/mandates/execute", 201, []byte(`{"code":0,"message":"Payment initiated","payment":{"payments_session_id":"28000000000500001","invoice_number":"att_1","customer_id":"28000000000400001","amount":"499.00","currency":"INR","status":"initiated","reference_number":"att_1","date":1760000000,"payment_method":{"type":"upi"}}}`))
	return f
}

func chargeReq() connector.ChargeRequest {
	return connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "28000000000700001"}, ProviderRef: "att_1", Amount: 49900, Currency: "INR"}
}

func TestChargeMandateRunsAnExecutionSession(t *testing.T) {
	f := executeFake()
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	s, err := c.ChargeMandate(context.Background(), chargeReq())
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != connector.PaymentProcessing || s.ProviderRef != "att_1" || s.ProviderOrderID != "28000000000500001" || s.Amount != 49900 || s.Currency != "INR" {
		t.Errorf("%+v", s)
	}
	var sess map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/paymentsessions"], &sess)
	if sess["type"] != "mandate_execution" || sess["customer_id"] != "28000000000400001" || sess["invoice_number"] != "att_1" || sess["amount"] != 499.0 || sess["max_retry_count"] != 1.0 {
		t.Errorf("execution session = %v", sess)
	}
	var ex map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/mandates/execute"], &ex)
	if ex["payments_session_id"] != float64(28000000000500001) || ex["mandate_id"] != float64(28000000000700001) || ex["customer_id"] != float64(28000000000400001) ||
		ex["invoice_number"] != "att_1" || ex["reference_number"] != "att_1" || ex["amount"] != 499.0 {
		t.Errorf("execute body = %v", ex)
	}
	// A repeated charge with the same reference debits nothing more.
	f.on("GET /api/v1/paymentsessions/28000000000500001", 200, []byte(`{"code":0,"payments_session":{"payments_session_id":"28000000000500001","status":"created","payments":[{"payment_id":"28000000000900001","status":"initiated"}]}}`))
	f.on("GET /api/v1/payments/28000000000900001", 200, []byte(`{"code":0,"payment":{"payment_id":"28000000000900001","payments_session_id":"28000000000500001","reference_number":"att_1","amount":"499.00","currency":"INR","status":"initiated","payment_method":{"type":"upi"}}}`))
	again, err := c.ChargeMandate(context.Background(), chargeReq())
	if err != nil || again.ProviderPaymentID != "28000000000900001" || again.ProviderRef != "att_1" {
		t.Fatalf("%+v %v", again, err)
	}
	n := 0
	for _, call := range f.calls {
		if call == "POST /api/v1/mandates/execute" || call == "POST /api/v1/paymentsessions" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("one session and one execute for two identical charges, got %d requests: %v", n, f.calls)
	}
	// The poller asks by Paymux's reference only: it is resolved through the session.
	snap, err := c.(connector.Connector).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "att_1"})
	if err != nil || snap.ProviderPaymentID != "28000000000900001" || snap.Status != connector.PaymentProcessing {
		t.Errorf("FetchPayment by reference: %+v %v", snap, err)
	}
}

// The notice's id links the execution to the notice sent for it.
func TestChargeMandateUsesTheNoticeSent(t *testing.T) {
	f := executeFake()
	f.on("POST /api/v1/mandates/notify", 201, []byte(`{"code":0,"mandate_notification":{"mandate_notification_id":"28000000000800001","notification_status":"succeeded"}}`))
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	if err := c.NotifyPreDebit(context.Background(), connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "28000000000700001"}, ProviderRef: "att_1", Amount: 49900, Currency: "INR", DebitDate: (mandateNow + 48*3600) * 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChargeMandate(context.Background(), chargeReq()); err != nil {
		t.Fatal(err)
	}
	var ex map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/mandates/execute"], &ex)
	if ex["mandate_notification_id"] != float64(28000000000800001) {
		t.Errorf("execute body = %v", ex)
	}
}

func TestChargeMandateRules(t *testing.T) {
	ctx := context.Background()
	f := executeFake()
	c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
	// RBI: an unattended UPI debit is capped at Rs 15,000 (above it each debit needs the customer's authentication).
	over := chargeReq()
	over.Amount = 1500100
	if _, err := c.ChargeMandate(ctx, over); !errors.Is(err, connector.ErrMandateLimit) {
		t.Errorf("above Rs 15,000: %v", err)
	}
	// Above the mandate's own maximum.
	big := chargeReq()
	big.Amount = 160000
	if _, err := c.ChargeMandate(ctx, big); !errors.Is(err, connector.ErrMandateLimit) {
		t.Errorf("above the mandate's maximum: %v", err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "POST") {
			t.Errorf("a refused debit reached Zoho: %v", f.calls)
		}
	}
	// A mandate that is not active is a declined debit, not an error.
	g := executeFake()
	g.on("GET /api/v1/mandates/28000000000700001", 200, []byte(`{"code":0,"mandate":{"mandate_id":"28000000000700001","customer_id":"28000000000400001","amount":"1499.00","currency":"INR","status":"inactive"}}`))
	s, err := g.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager).ChargeMandate(ctx, chargeReq())
	if err != nil || s.Status != connector.PaymentFailed || s.ErrorCode == "" {
		t.Errorf("inactive mandate: %+v %v", s, err)
	}
	// NPCI runs executions before 10:00, 13:00-17:00 and after 21:30 IST; in the
	// closed windows the debit waits (retryable) instead of failing.
	h := executeFake()
	closed := time.Date(2025, 10, 9, 11, 0, 0, 0, time.FixedZone("IST", 5*3600+1800)) // 11:00 IST
	_, err = h.connAt(t, widgetCfg, closed).(connector.MandateManager).ChargeMandate(ctx, chargeReq())
	if !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("in NPCI's closed window: %v", err)
	}
	for _, call := range h.calls {
		if strings.HasPrefix(call, "POST") {
			t.Errorf("a debit in the closed window reached Zoho: %v", h.calls)
		}
	}
}

// Zoho has no API to revoke a mandate: the customer ends it in their UPI app and
// the mandate.inactive event reports it.
func TestRevokeMandateIsUnsupported(t *testing.T) {
	c := newRFake().conn(t, widgetCfg).(connector.MandateManager)
	if err := c.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("%v", err)
	}
}

func TestInfoDeclaresTheUPICapAndPerAttemptNotices(t *testing.T) {
	i := newRFake().conn(t, widgetCfg).Info()
	if !i.PreDebitPerAttempt {
		t.Error("each execution session needs its own notice")
	}
	if len(i.MandateLimits) != 1 || i.MandateLimits[0].Method != "upi" || i.MandateLimits[0].MaxAmount != 1500000 {
		t.Errorf("%+v", i.MandateLimits)
	}
}

// Zoho's help pages: a future-dated mandate is authorised with exactly Rs 1, a
// same-day one needs at least Rs 6; nothing in between is accepted, and the Rs 1
// is settled to the merchant in the next payout (Zoho does not reverse it).
func TestMandateEnrollmentAmountRules(t *testing.T) {
	for amt, ok := range map[int64]bool{50: false, 99: false, 100: true, 101: false, 250: false, 599: false, 600: true, 49900: true} {
		f := mandateFake()
		c := f.connAt(t, widgetCfg, time.Unix(mandateNow, 0)).(connector.MandateManager)
		r := mandateReq()
		r.Amount = amt
		_, err := c.CreateMandateCheckout(context.Background(), r)
		if (err == nil) != ok {
			t.Errorf("amount %d: err = %v, want accepted=%v", amt, err, ok)
		}
		if !ok && len(f.calls) != 0 {
			t.Errorf("amount %d: requests for a refused mandate: %v", amt, f.calls)
		}
	}
}

func TestInfoDoesNotClaimTheVerificationDebitIsReversed(t *testing.T) {
	i := newRFake().conn(t, widgetCfg).Info()
	if i.RefundsVerificationDebit {
		t.Error("Zoho settles the Rs 1 authorisation to the merchant's payout; Paymux must refund it")
	}
	if i.MandateSetupAmount != 100 {
		t.Errorf("MandateSetupAmount = %d", i.MandateSetupAmount)
	}
}
