package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

func mgr(t *testing.T, f *fake, cfg json.RawMessage) connector.MandateManager {
	t.Helper()
	m, ok := newConn(t, f, cfg).(connector.MandateManager)
	if !ok {
		t.Fatal("zoho does not implement MandateManager")
	}
	return m
}

func TestInfoDeclaresUPIMandates(t *testing.T) {
	i := newConn(t, newFake(""), widgetCfg).Info()
	if !i.Has(connector.CapMandateManager) || len(i.SubscriptionModes) != 1 || i.SubscriptionModes[0] != connector.MerchantTriggered ||
		len(i.MandateLimits) != 1 || i.MandateLimits[0].Method != "upi" || i.PreDebitPerAttempt {
		t.Errorf("info = %+v", i)
	}
}

func TestMandateCheckoutWidgetAndHosted(t *testing.T) {
	f := newFake("")
	req := connector.MandateRequest{MandateID: "mdt_1", ProviderRef: "MANDREF01", Amount: 49900, MaxAmount: 74850, Currency: "INR", Method: "upi",
		ValidUntil: time.Date(2032, 1, 31, 4, 0, 0, 0, time.UTC).UnixMilli(), Customer: connector.Customer{Name: "Asha", Email: "a@example.com", Phone: "9999999999"}}
	res, err := mgr(t, f, widgetCfg).CreateMandateCheckout(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Amount    json.Number `json:"amount"`
		MaxAmount json.Number `json:"max_amount"`
		Reference string      `json:"reference_number"`
		ReturnURL string      `json:"return_url"`
		EndDate   string      `json:"end_date"`
		Frequency string      `json:"frequency"`
	}
	_ = json.Unmarshal(f.bodies["POST /api/v1/mandates"], &body)
	if body.Amount.String() != "499.00" || body.MaxAmount.String() != "748.50" || body.Reference != "MANDREF01" || body.ReturnURL != "https://pay.example.com/return/acc/MANDREF01" ||
		body.EndDate != "2032-01-31" || body.Frequency != "as_presented" {
		t.Errorf("body = %s", f.bodies["POST /api/v1/mandates"])
	}
	na := res.NextAction
	if res.ProviderOrderID != "md_z1" || na.Kind != connector.ActionSDK || na.Options["payments_session_id"] != "ps_m1" || na.Options["api_key"] != "wkey" || na.Options["amount"] != "748.50" {
		t.Errorf("res = %+v", res)
	}
	res, err = mgr(t, newFake(""), hostedCfg).CreateMandateCheckout(context.Background(), req)
	if err != nil || res.NextAction.Kind != connector.ActionRedirect || res.NextAction.URL != "https://pay.zoho.example.com/checkout?payments_session_id=ps_m1" {
		t.Errorf("hosted: %+v %v", res, err)
	}
	for _, bad := range []connector.MandateRequest{
		{ProviderRef: "R", MaxAmount: 100, Currency: "USD", Method: "upi"},
		{ProviderRef: "R", MaxAmount: 100, Currency: "INR", Method: "card"},
		{ProviderRef: "", MaxAmount: 100, Currency: "INR", Method: "upi"},
	} {
		if _, err := mgr(t, newFake(""), widgetCfg).CreateMandateCheckout(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestFetchMandateStatuses(t *testing.T) {
	f := newFake("")
	m := mgr(t, f, widgetCfg)
	for raw, want := range map[string]string{"created": "pending", "active": "active", "revoked": "revoked", "expired": "expired", "failed": "failed", "weird": "pending"} {
		f.mandateStat = raw
		s, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "md_z1"})
		if err != nil || s.Status != want || s.RawStatus != raw || s.MaxAmount != 74850 || s.ProviderMandateID != "md_z1" || s.Method != "upi" {
			t.Errorf("%s: %+v %v", raw, s, err)
		}
		if raw == "active" {
			if sp := s.SetupPayment; sp == nil || sp.Status != connector.PaymentSucceeded || sp.Amount != 49900 || sp.ProviderPaymentID != "pay_zreg" || sp.ProviderRef != "MANDREF01" {
				t.Errorf("setup payment = %+v", s.SetupPayment)
			}
		} else if s.SetupPayment != nil {
			t.Errorf("%s: unexpected setup payment %+v", raw, s.SetupPayment)
		}
	}
	if _, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "nope"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestNoPreDebitNoticeAPI(t *testing.T) {
	if err := mgr(t, newFake(""), widgetCfg).NotifyPreDebit(context.Background(), connector.PreDebitRequest{}); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

func TestChargeMandatePassesProviderRef(t *testing.T) {
	f := newFake("")
	m := mgr(t, f, widgetCfg)
	req := connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "md_z1"}, ProviderRef: "DEBITREF01", Amount: 49900, Currency: "INR"}
	s, err := m.ChargeMandate(context.Background(), req)
	if err != nil || s.Status != connector.PaymentProcessing || s.ProviderRef != "DEBITREF01" || s.ProviderPaymentID != "pay_zm1" || s.Amount != 49900 {
		t.Fatalf("snap = %+v %v", s, err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/mandates/md_z1/charges"], &body)
	if body["reference_number"] != "DEBITREF01" || body["currency"] != "INR" || body["amount"] != 499.0 {
		t.Errorf("body = %v", body)
	}
	f.chargeCode, f.chargeMsg = 400, "Mandate is not active"
	s, err = m.ChargeMandate(context.Background(), req)
	if err != nil || s.Status != connector.PaymentFailed || s.ErrorCode != "mandate_rejected" {
		t.Errorf("rejected: %+v %v", s, err)
	}
	f.chargeMsg = "Amount exceeds the mandate limit"
	if _, err := m.ChargeMandate(context.Background(), req); !errors.Is(err, connector.ErrMandateLimit) {
		t.Errorf("limit: %v", err)
	}
	f.chargeCode = 429
	var rl connector.ErrRateLimited
	if _, err := m.ChargeMandate(context.Background(), req); !errors.As(err, &rl) {
		t.Errorf("rate limit: %v", err)
	}
	if _, err := m.ChargeMandate(context.Background(), connector.ChargeRequest{Amount: 1, Currency: "INR"}); err == nil {
		t.Error("charge without provider_ref accepted")
	}
}

func TestRevokeMandate(t *testing.T) {
	f := newFake("")
	m := mgr(t, f, widgetCfg)
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "md_z1"}); err != nil {
		t.Fatal(err)
	}
	f.mandateStat = "revoked"
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "md_z1"}); err != nil {
		t.Errorf("already revoked: %v", err)
	}
}

func TestMandateWebhook(t *testing.T) {
	c := newConn(t, newFake(""), widgetCfg)
	body := `{"event_id":1003,"event_type":"mandate.revoked","event_object":{"mandate":{"mandate_id":"md_z1","status":"revoked","max_amount":"748.50","currency":"INR"}}}`
	ns, err := c.ParseWebhook(context.Background(), signedWebhook(testSecret, body, nowUnix, nowUnix))
	if err != nil || len(ns) != 1 {
		t.Fatalf("%v %v", ns, err)
	}
	n := ns[0]
	if n.Kind != connector.NotifyMandate || n.EventID != "1003" || n.Mandate.Status != "revoked" || n.Mandate.ProviderMandateID != "md_z1" || n.Mandate.MaxAmount != 74850 {
		t.Errorf("notification = %+v", n)
	}
}
