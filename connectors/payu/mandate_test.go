package payu

import (
	"context"
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Independent Python hashlib values for the layouts in mandate.go.
const (
	goldenSIDetails = `{"billingAmount":"748.50","billingCurrency":"INR","billingCycle":"ADHOC","billingInterval":1,"paymentStartDate":"2027-01-31","paymentEndDate":"2032-01-31"}`
	goldenSIHash    = "075902a9417c8d323af7b0bc555760774a544467acbdb5e391bd2a3300411cb25afd43d89402826bb8b3d010cba17a9ae0283a1ef2d833c88ab949dafe7cda6b"
	goldenSIHash499 = "75abf9274c220729dae76bdaaefd4c86c44abfff0ff1b335fb2047b7325fd4fb569a5c2cbd58f9b96119d1d135f74dc0be3e89812c40b4f99f3e2e92bea2037a"
	// var1 field names follow PayU's documentation and were accepted by the test environment.
	goldenPreVar1   = `{"authpayuid":"403993715521","requestId":"DEBITREF01","amount":"499.00","debitDate":"2027-02-28","invoiceDisplayNumber":"DEBITREF01"}`
	goldenPreHash   = "5583d8f48e909e09e032bdd3c4614624db0992bc8dea5f4801dcf1243b437e887fd4befa17b8a6bf927fd27e323995b818e561d3d2b142a0cb3f26d1a442073b"
	goldenChargeVar = `{"authpayuid":"403993715521","invoiceDisplayNumber":"DEBITREF01","amount":"499.00","txnid":"DEBITREF01"}`
	goldenChargeHsh = "8bece0b04dfcdfa13ca7d66fe13016ecb3d0f688cccb1269c69970ff2c57df71438990ecc16d854b541c9425d8a44f58cab9375807a334f3f935ee45101c0773"
)

var siNow = time.Date(2027, 1, 31, 4, 0, 0, 0, time.UTC)

func mandateConn(t *testing.T, f *fake) connector.MandateManager {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(testConfig, connectortest.NewDeps(srv, siNow))
	if err != nil {
		t.Fatal(err)
	}
	return c.(connector.MandateManager)
}

func TestInfoDeclaresMandates(t *testing.T) {
	i := newConn(t, &fake{}).Info()
	if i.MandateSetupAmount != 100 {
		t.Errorf("verification amount = %d", i.MandateSetupAmount)
	}
	if !i.Has(connector.CapMandateManager) || len(i.SubscriptionModes) != 1 || i.SubscriptionModes[0] != connector.MerchantTriggered || !i.PreDebitPerAttempt {
		t.Errorf("info = %+v", i)
	}
	var emandate int64
	for _, l := range i.MandateLimits {
		if l.Method == "emandate" {
			emandate = l.MaxAmount
		}
	}
	if emandate != 1_000_000_000 {
		t.Errorf("e-NACH ceiling = %d, want Rs 1 crore in paise", emandate)
	}
}

func TestMandateCheckoutFormAndGoldenSIHash(t *testing.T) {
	m := mandateConn(t, &fake{})
	res, err := m.CreateMandateCheckout(context.Background(), connector.MandateRequest{MandateID: "mdt_01", ProviderRef: "MANDREF01",
		MaxAmount: 74850, Currency: "INR", Method: "upi", ValidUntil: time.Date(2032, 1, 31, 4, 0, 0, 0, time.UTC).UnixMilli(),
		Customer: connector.Customer{Name: "Asha", Email: "asha@example.com", Phone: "9999999999"}})
	if err != nil {
		t.Fatal(err)
	}
	na := res.NextAction
	if na.Kind != connector.ActionFormPost || na.URL != "https://test.payu.in/_payment" {
		t.Fatalf("action = %+v", na)
	}
	f := na.Fields
	if f["si"] != "1" || f["api_version"] != "7" || f["pg"] != "UPI" || f["txnid"] != "MANDREF01" || f["amount"] != "1.00" ||
		f["udf1"] != "mdt_01" || f["surl"] != "https://pay.example.com/return/acc/MANDREF01" || f["si_details"] != goldenSIDetails || f["hash"] != goldenSIHash {
		t.Errorf("fields = %+v", f)
	}
	// The registration debits the first period itself.
	first, err := m.CreateMandateCheckout(context.Background(), connector.MandateRequest{MandateID: "mdt_01", ProviderRef: "MANDREF01",
		Amount: 49900, MaxAmount: 74850, Currency: "INR", Method: "upi", ValidUntil: time.Date(2032, 1, 31, 4, 0, 0, 0, time.UTC).UnixMilli(),
		Customer: connector.Customer{Name: "Asha", Email: "asha@example.com"}})
	if err != nil || first.NextAction.Fields["amount"] != "499.00" || first.NextAction.Fields["hash"] != goldenSIHash499 {
		t.Errorf("first-period registration: %+v %v", first.NextAction.Fields, err)
	}
	for method, pg := range map[string]string{"card": "CC", "emandate": "NB"} {
		r, err := m.CreateMandateCheckout(context.Background(), connector.MandateRequest{ProviderRef: "R", MaxAmount: 100, Currency: "INR", Method: method})
		if err != nil || r.NextAction.Fields["pg"] != pg {
			t.Errorf("%s: %+v %v", method, r.NextAction.Fields, err)
		}
	}
	for _, bad := range []connector.MandateRequest{
		{ProviderRef: "R", MaxAmount: 100, Currency: "USD", Method: "upi"},
		{ProviderRef: "R", MaxAmount: 100, Currency: "INR", Method: "wallet"},
		{ProviderRef: "", MaxAmount: 100, Currency: "INR", Method: "upi"},
		{ProviderRef: "R", MaxAmount: 0, Currency: "INR", Method: "upi"},
	} {
		if _, err := m.CreateMandateCheckout(context.Background(), bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

var requestIDRE = regexp.MustCompile(`^\{"authpayuid":"[0-9]+","requestId":"[A-Za-z0-9]+"\}$`)

func TestFetchMandateByRegistrationRefThenById(t *testing.T) {
	f := &fake{verify: verifyBody("success", "captured"), cmds: map[string]string{
		"check_mandate_status": `{"status":"deleted","action":"check_mandate_status","authpayuid":"403993715521","amount":1500,"mandateStartDate":"2026-10-18","mandateEndDate":"2031-10-18","deletedon":""}`}}
	m := mandateConn(t, f)
	// Right after the return only the txnid is known: it yields the mihpayid.
	s, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "pay_ref_01"})
	if err != nil || s.Status != "active" || s.ProviderMandateID != "403993715521" {
		t.Fatalf("by ref: %+v %v", s, err)
	}
	// The registration transaction rides along: it is the first period's payment.
	if sp := s.SetupPayment; sp == nil || sp.Status != connector.PaymentSucceeded || sp.Amount != 49900 || sp.ProviderPaymentID != "403993715521" || sp.ProviderRef != "pay_ref_01" {
		t.Errorf("setup payment = %+v", s.SetupPayment)
	}
	f.verify = verifyBody("failure", "failed")
	if s, _ := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "pay_ref_01"}); s.Status != "failed" {
		t.Errorf("failed registration: %+v", s)
	}
	// Later by the mandate's own id.
	s, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"})
	if err != nil || s.Status != "revoked" || s.ProviderMandateID != "403993715521" {
		t.Fatalf("by id: %+v %v", s, err)
	}
	// check_mandate_status and mandate_revoke refuse a call without a requestId
	// ("requestId is mandatory", verified live): a fresh one is sent each time.
	if got := f.last(); got.Get("command") != "check_mandate_status" || !requestIDRE.MatchString(got.Get("var1")) || !strings.Contains(got.Get("var1"), `"authpayuid":"403993715521"`) {
		t.Errorf("form = %v", got)
	}
	first := f.last().Get("var1")
	_, _ = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"})
	if f.last().Get("var1") == first {
		t.Error("the requestId was reused")
	}
	f.cmds["check_mandate_status"] = `{"status":0,"msg":"no such mandate"}`
	if _, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	// These commands name their message "message", not "msg" (verified live).
	f.cmds["check_mandate_status"] = `{"status":0,"message":"Consent is Not Mandated","action":"check_mandate_status"}`
	if _, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown mandate: %v", err)
	}
	f.cmds["check_mandate_status"] = `{"status":0,"message":"Invalid Hash."}`
	if _, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("bad hash: %v", err)
	}
}

func TestPreDebitNoticeGolden(t *testing.T) {
	f := &fake{cmds: map[string]string{"pre_debit_SI": `{"status":1,"msg":"Pre debit notification sent"}`}}
	m := mandateConn(t, f)
	err := m.NotifyPreDebit(context.Background(), connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "403993715521"},
		ProviderRef: "DEBITREF01", Amount: 49900, Currency: "INR", DebitDate: time.Date(2027, 2, 28, 4, 0, 0, 0, time.UTC).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	got := f.last()
	if got.Get("command") != "pre_debit_SI" || got.Get("var1") != goldenPreVar1 || got.Get("hash") != goldenPreHash {
		t.Errorf("form = %v", got)
	}
	f.cmds["pre_debit_SI"] = `{"status":0,"msg":"Notification already sent"}`
	if err := m.NotifyPreDebit(context.Background(), connector.PreDebitRequest{Currency: "INR", Amount: 100}); err != nil {
		t.Errorf("repeat notice: %v", err)
	}
	f.cmds["pre_debit_SI"] = `{"status":0,"msg":"Mandate not active"}`
	if err := m.NotifyPreDebit(context.Background(), connector.PreDebitRequest{Currency: "INR", Amount: 100}); err == nil {
		t.Error("refused notice accepted")
	}
}

func TestChargeMandatePassesProviderRefForIdempotency(t *testing.T) {
	f := &fake{cmds: map[string]string{"si_transaction": `{"status":1,"message":"Transaction Processed successfully","details":{"DEBITREF01":{"authpayuid":"403993715521","transactionid":"DEBITREF01","amount":"499.00","payuid":"","status":"pending","field9":""}}}`}}
	m := mandateConn(t, f)
	req := connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "403993715521"}, ProviderRef: "DEBITREF01", Amount: 49900, Currency: "INR"}
	snap, err := m.ChargeMandate(context.Background(), req)
	if err != nil || snap.Status != connector.PaymentProcessing || snap.ProviderRef != "DEBITREF01" {
		t.Fatalf("queued: %+v %v", snap, err)
	}
	got := f.last()
	if got.Get("command") != "si_transaction" || got.Get("var1") != goldenChargeVar || got.Get("hash") != goldenChargeHsh {
		t.Errorf("form = %v (transactionId must be the provider_ref)", got)
	}

	// Immediate result in the answer.
	f.cmds["si_transaction"] = `{"status":1,"message":"Transaction Processed successfully","details":{"DEBITREF01":{"authpayuid":"403993715521","transactionid":"DEBITREF01","amount":"499.00","payuid":"7001","status":"captured","field9":"Transaction Completed Successfully"}}}`
	snap, err = m.ChargeMandate(context.Background(), req)
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.Amount != 49900 || snap.ProviderPaymentID != "7001" {
		t.Fatalf("immediate: %+v %v", snap, err)
	}

	// PayU refuses a repeated transactionId: answer with the first debit's state.
	f.cmds["si_transaction"] = `{"status":0,"msg":"Duplicate transactionId"}`
	f.verify = verifyBody("success", "captured")
	snap, err = m.ChargeMandate(context.Background(), connector.ChargeRequest{Mandate: req.Mandate, ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR"})
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderPaymentID != "403993715521" {
		t.Fatalf("duplicate: %+v %v", snap, err)
	}

	// A declined debit is a failed snapshot, not an error; the limit is an error.
	f.cmds["si_transaction"] = `{"status":0,"msg":"Insufficient funds in the account"}`
	snap, err = m.ChargeMandate(context.Background(), req)
	if err != nil || snap.Status != connector.PaymentFailed || snap.ErrorMessage == "" {
		t.Fatalf("declined: %+v %v", snap, err)
	}
	f.cmds["si_transaction"] = `{"status":0,"msg":"Amount exceeds the mandate limit"}`
	if _, err := m.ChargeMandate(context.Background(), req); !errors.Is(err, connector.ErrMandateLimit) {
		t.Errorf("limit: %v", err)
	}
	if _, err := m.ChargeMandate(context.Background(), connector.ChargeRequest{ProviderRef: "", Amount: 1, Currency: "INR"}); err == nil {
		t.Error("charge without provider_ref accepted")
	}
}

func TestRevokeMandate(t *testing.T) {
	f := &fake{cmds: map[string]string{"mandate_revoke": `{"status":1,"msg":"revoked"}`}}
	m := mandateConn(t, f)
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"}); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.Get("command") != "mandate_revoke" || !requestIDRE.MatchString(got.Get("var1")) || !strings.Contains(got.Get("var1"), `"authpayuid":"403993715521"`) {
		t.Errorf("form = %v", got)
	}
	f.cmds["mandate_revoke"] = `{"status":0,"message":"Mandate entry not found","action":"MANDATE_REVOKE"}`
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown mandate (message key): %v", err)
	}
	f.cmds["mandate_revoke"] = `{"status":0,"msg":"Mandate already cancelled"}`
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); err != nil {
		t.Errorf("already revoked: %v", err)
	}
	f.cmds["mandate_revoke"] = `{"status":0,"msg":"Mandate not found"}`
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}
