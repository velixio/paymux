package payu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// Answers recorded from the PayU test environment (2026-10-18), with a card
// registration made through the SI form post (PayU showed "Subscription Plan,
// Maximum Mandate Amount, As Presented" for the form this connector builds, so
// the si_details and the registration hash are right).

const (
	statusPending  = `{"status":"pending","action":"check_mandate_status","authpayuid":"403993715521","amount":1500,"mandateStartDate":"2026-10-18","mandateEndDate":"2031-10-18"}`
	statusActive   = `{"status":"active","action":"check_mandate_status","authpayuid":"403993715521","amount":"1500.00","mandateStartDate":"2026-10-18","mandateEndDate":"2031-10-18"}`
	statusNotFound = `{"status":0,"message":"Consent is Not Mandated","action":"check_mandate_status"}`
	upiNotFound    = `{"status":0,"message":"Mandate entry not found","action":"MANDATE_STATUS"}`
	nbNotFound     = `{"status":0,"message":"ENACH Mandate entry not found","action":"NB_mandate_status"}`
)

func TestRealMandateStatuses(t *testing.T) {
	f := &fake{cmds: map[string]string{"check_mandate_status": statusPending}}
	m := mandateConn(t, f)
	s, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"})
	if err != nil || s.Status != "pending" || s.RawStatus != "pending" || s.MaxAmount != 150000 || s.Method != "card" || s.ValidUntil == 0 {
		t.Fatalf("pending: %+v %v", s, err)
	}
	f.cmds["check_mandate_status"] = statusActive
	if s, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"}); err != nil || s.Status != "active" {
		t.Fatalf("active: %+v %v", s, err)
	}
	for raw, want := range map[string]string{"cancelled": "revoked", "discarded": "revoked", "deleted": "revoked", "expired": "expired", "failed": "failed", "paused": "pending"} {
		f.cmds["check_mandate_status"] = `{"status":"` + raw + `","action":"check_mandate_status","authpayuid":"403993715521","amount":1500}`
		if s, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"}); err != nil || s.Status != want {
			t.Errorf("%s: %+v %v (want %s)", raw, s, err, want)
		}
	}
}

// A UPI or net-banking mandate is unknown to the card command ("Consent is Not
// Mandated"): the UPI and net-banking status commands are asked next.
func TestRealMandateStatusFallsBackToUPIAndNetBanking(t *testing.T) {
	f := &fake{cmds: map[string]string{"check_mandate_status": statusNotFound,
		"upi_mandate_status": `{"status":"active","action":"MANDATE_STATUS","authpayuid":"403993715521","amount":"749.00","mandateStartDate":"2026-10-18","mandateEndDate":"2027-10-18"}`}}
	m := mandateConn(t, f)
	s, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"})
	if err != nil || s.Status != "active" || s.Method != "upi" || s.MaxAmount != 74900 {
		t.Fatalf("upi: %+v %v", s, err)
	}
	f.cmds["upi_mandate_status"] = upiNotFound
	f.cmds["NB_mandate_status"] = `{"status":"active","action":"NB_mandate_status","authpayuid":"403993715521","amount":"749.00"}`
	if s, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"}); err != nil || s.Method != "emandate" {
		t.Fatalf("net banking: %+v %v", s, err)
	}
	f.cmds["NB_mandate_status"] = nbNotFound
	if _, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown everywhere: %v", err)
	}
}

// si_transaction answers status 1 for a debit it processed, with the outcome of
// the debit under "details": "captured", or "failed" and field9 saying why
// (e.g. "entry not found in si_txn", "Invalid/missing card token").
func TestRealSIDebitOutcomes(t *testing.T) {
	f := &fake{cmds: map[string]string{"si_transaction": `{"status":1,"message":"Transaction Processed successfully","details":{"DEBITREF01":{"authpayuid":"403993715521","transactionid":"DEBITREF01","amount":"499.00","user_credentials":"","card_token":"","payuid":"","status":"failed","udf1":"","field9":"Invalid\/missing card token","phone":"","email":"","fileName":""}}}`}}
	m := mandateConn(t, f)
	req := connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "403993715521"}, ProviderRef: "DEBITREF01", Amount: 49900, Currency: "INR"}
	s, err := m.ChargeMandate(context.Background(), req)
	if err != nil || s.Status != connector.PaymentFailed || !strings.Contains(s.ErrorMessage, "card token") || s.ProviderRef != "DEBITREF01" || s.Amount != 49900 {
		t.Fatalf("failed debit: %+v %v", s, err)
	}
	f.cmds["si_transaction"] = `{"status":1,"message":"Transaction Processed successfully","details":{"DEBITREF01":{"authpayuid":"403993715521","transactionid":"DEBITREF01","amount":"499.00","payuid":"25600342065","status":"captured","field9":"Transaction Completed Successfully"}}}`
	s, err = m.ChargeMandate(context.Background(), req)
	if err != nil || s.Status != connector.PaymentSucceeded || s.ProviderPaymentID != "25600342065" || s.ErrorMessage != "" {
		t.Fatalf("captured: %+v %v", s, err)
	}
}

func TestRealRevokeAndPreDebitAnswers(t *testing.T) {
	f := &fake{cmds: map[string]string{
		"mandate_revoke": `{"status":1,"message":"Mandate Revoked Successfully","action":"MANDATE_REVOKE"}`,
		"pre_debit_SI":   `{"status":1,"action":"MANDATE_PRE_DEBIT","message":"Request Processed Successfully"}`}}
	m := mandateConn(t, f)
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "403993715521"}); err != nil {
		t.Errorf("revoke: %v", err)
	}
	f.cmds["mandate_revoke"] = `{"status":0,"message":"Mandate entry not found","action":"MANDATE_REVOKE"}`
	if err := m.RevokeMandate(context.Background(), connector.MandateRef{ProviderMandateID: "1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	pd := connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "403993715521"}, ProviderRef: "D1", Amount: 100, Currency: "INR", DebitDate: siNow.UnixMilli()}
	if err := m.NotifyPreDebit(context.Background(), pd); err != nil {
		t.Errorf("notice: %v", err)
	}
	f.cmds["pre_debit_SI"] = `{"status":0,"message":"Consent is Not Enrolled Or Not IN Active State","action":"MANDATE_PRE_DEBIT"}`
	if err := m.NotifyPreDebit(context.Background(), pd); err == nil {
		t.Error("a refused notice was accepted")
	}
}
