package payu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
)

// These tests pin the behaviour PayU's recurring-payments guide
// (docs.payu.in, "Subscriptions or Recurring Payments") asks for.

// calls returns the var1 of every postservice call of a command, in order.
func (f *fake) calls(cmd string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, form := range f.forms {
		if form.Get("command") != cmd {
			continue
		}
		var v map[string]any
		_ = json.Unmarshal([]byte(form.Get("var1")), &v)
		out = append(out, v)
	}
	return out
}

func (f *fake) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, form := range f.forms {
		if c := form.Get("command"); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// registration is a verify_payment answer for a registration transaction.
func registration(status, source, mode string) string {
	return `{"status":1,"msg":"1 out of 1 Transactions Fetched Successfully","transaction_details":{"MANDREF01":{"mihpayid":"20000064468","txnid":"MANDREF01","status":"` + status +
		`","unmappedstatus":"captured","amt":"10.00","mode":"` + mode + `","payment_source":"` + source + `","error_code":"E000","error_Message":"NO ERROR","addedon":"2026-10-18 09:45:29"}}}`
}

// UPI AutoPay debits are limited to Rs 15,000 by default (the guide's si_details
// page: "UPI cap: INR 15000").
func TestUPIMandateCapIsFifteenThousand(t *testing.T) {
	for _, l := range newConn(t, &fake{}).Info().MandateLimits {
		if l.Method == "upi" && l.MaxAmount != 1_500_000 {
			t.Errorf("upi cap = %d, want Rs 15,000 in paise", l.MaxAmount)
		}
		if l.Method == "upi" {
			return
		}
	}
	t.Error("no upi limit declared")
}

// The pre-debit notice must come at least 48 hours ahead of a card or UPI debit.
func TestPreDebitNoticeLead(t *testing.T) {
	if h := newConn(t, &fake{}).Info().PreDebitMinHours; h != 48 {
		t.Errorf("PreDebitMinHours = %d, want 48", h)
	}
}

// pg codes: the guide names ENACH for net-banking e-mandates (not NB), and
// bankcode UPI for UPI.
func TestRegistrationPaymentCodes(t *testing.T) {
	m := mandateConn(t, &fake{})
	for method, want := range map[string][2]string{"upi": {"UPI", "UPI"}, "card": {"CC", ""}, "emandate": {"ENACH", ""}} {
		res, err := m.CreateMandateCheckout(context.Background(), connector.MandateRequest{MandateID: "mdt_01", ProviderRef: "MANDREF01",
			MaxAmount: 50000, Currency: "INR", Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if f := res.NextAction.Fields; f["pg"] != want[0] || f["bankcode"] != want[1] {
			t.Errorf("%s: pg=%q bankcode=%q, want %v", method, f["pg"], f["bankcode"], want)
		}
	}
}

// A consent is valid only when payment_source is "sist". A registration whose
// transaction succeeded but whose payment_source is "payu" created no mandate
// (PayU's outcome table: Mandate failed, Transaction successful), so the
// debits that follow would all fail.
func TestRegistrationNeedsSISPaymentSource(t *testing.T) {
	f := &fake{cmds: map[string]string{"verify_payment": registration("success", "payu", "CC")}}
	m := mandateConn(t, f)
	s, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "MANDREF01"})
	if err != nil || s.Status != "failed" || s.ProviderMandateID != "20000064468" {
		t.Fatalf("no mandate behind a payu-sourced payment: %+v %v", s, err)
	}
	if s.SetupPayment == nil || s.SetupPayment.Status != connector.PaymentSucceeded {
		t.Errorf("the captured registration payment must still be reported (it is refunded): %+v", s.SetupPayment)
	}
	for _, src := range []string{"sist", "SIST"} {
		f.cmds["verify_payment"] = registration("success", src, "CC")
		if s, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "MANDREF01"}); err != nil || s.Status != "active" {
			t.Errorf("source %s: %+v %v", src, s, err)
		}
	}
}

// The registration's mode says which status command applies; it is asked first
// and the mode is reported as the mandate's method.
func TestRegistrationModeSelectsStatusCommand(t *testing.T) {
	for mode, want := range map[string][2]string{"UPI": {"upi_mandate_status", "upi"}, "ENACH": {"NB_mandate_status", "emandate"}, "CC": {"check_mandate_status", "card"}, "DC": {"check_mandate_status", "card"}} {
		f := &fake{cmds: map[string]string{"verify_payment": registration("success", "sist", mode),
			"upi_mandate_status":   `{"status":"active","action":"MANDATE_STATUS","authpayuid":"20000064468","amount":"1500.00"}`,
			"NB_mandate_status":    `{"status":"SUCCESS","action":"NB_mandate_status","authpayuid":"20000064468","amount":"1500.00"}`,
			"check_mandate_status": `{"status":"active","action":"check_mandate_status","authpayuid":"20000064468","amount":1500}`}}
		m := mandateConn(t, f)
		s, err := m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "MANDREF01"})
		if err != nil || s.Method != want[1] {
			t.Fatalf("%s: %+v %v", mode, s, err)
		}
		s, err = m.FetchMandate(context.Background(), connector.MandateRef{ProviderMandateID: "20000064468"})
		if err != nil || s.Status != "active" || s.Method != want[1] {
			t.Fatalf("%s by id: %+v %v", mode, s, err)
		}
		if cmds := f.commands(); cmds[len(cmds)-1] != want[0] || len(f.calls(want[0])) != 1 {
			t.Errorf("%s: commands %v, want only %s asked", mode, cmds, want[0])
		}
	}
}

// Every status the guide lists, for cards, UPI and net banking.
func TestMandateStatusVocabulary(t *testing.T) {
	for raw, want := range map[string]string{
		"active": "active", "SUCCESS": "active", "cancelled": "revoked", "discarded": "revoked", "deleted": "revoked", "failed": "failed", "expired": "expired",
		"paused": "pending", "pause": "pending", "revoked": "revoked", "unpause": "active",
		"CANCEL_SUCCESS": "revoked", "INITIATED": "pending", "FAILED": "failed",
		"CANCEL_INITIATED": "pending", "CANCEL_PENDING": "pending", "CANCEL_FAILED": "active", "CANCEL_INITIATION_FAILED": "active",
		"somethingnew": "pending",
	} {
		if got := mandateStatus(raw); got != want {
			t.Errorf("%s -> %s, want %s", raw, got, want)
		}
	}
}

func TestPreDebitIsPerMode(t *testing.T) {
	ctx := context.Background()
	pd := func(ref string) connector.PreDebitRequest {
		return connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "20000064468"}, ProviderRef: ref, Amount: 49900, Currency: "INR", DebitDate: siNow.UnixMilli()}
	}
	ok := `{"status":1,"action":"MANDATE_PRE_DEBIT","message":"Request Processed Successfully"}`

	// UPI: the invoice number too, and the execution sequence number counts from 2 (the
	// registration was execution 1); a repeated notice for the same debit keeps its number.
	f := &fake{cmds: map[string]string{"verify_payment": registration("success", "sist", "UPI"), "upi_mandate_status": `{"status":"active","authpayuid":"20000064468","amount":"1500.00"}`, "pre_debit_SI": ok}}
	m := mandateConn(t, f)
	if _, err := m.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: "MANDREF01"}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"D1", "D1", "D2"} {
		if err := m.NotifyPreDebit(ctx, pd(ref)); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.calls("pre_debit_SI")
	if len(calls) != 3 || calls[0]["mandateSeqNo"] != float64(2) || calls[1]["mandateSeqNo"] != float64(2) || calls[2]["mandateSeqNo"] != float64(3) {
		t.Errorf("UPI sequence numbers: %v", calls)
	}
	if calls[0]["invoiceDisplayNumber"] != "D1" || calls[0]["amount"] != "499.00" || calls[0]["debitDate"] == "" { // the test environment refuses a notice without an invoice number before it looks at the mode
		t.Errorf("UPI var1: %v", calls[0])
	}

	// Cards: an invoice number, no sequence number.
	f = &fake{cmds: map[string]string{"verify_payment": registration("success", "sist", "CC"), "check_mandate_status": `{"status":"active","authpayuid":"20000064468","amount":1500}`, "pre_debit_SI": ok}}
	m = mandateConn(t, f)
	_, _ = m.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: "MANDREF01"})
	if err := m.NotifyPreDebit(ctx, pd("D1")); err != nil {
		t.Fatal(err)
	}
	if c := f.calls("pre_debit_SI"); len(c) != 1 || c[0]["invoiceDisplayNumber"] != "D1" || c[0]["mandateSeqNo"] != nil {
		t.Errorf("card var1: %v", c)
	}

	// Net banking: PayU takes no pre-debit notice ("applies only to Cards and UPI").
	f = &fake{cmds: map[string]string{"verify_payment": registration("success", "sist", "ENACH"), "NB_mandate_status": `{"status":"SUCCESS","authpayuid":"20000064468","amount":"1500.00"}`, "pre_debit_SI": ok}}
	m = mandateConn(t, f)
	_, _ = m.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: "MANDREF01"})
	if err := m.NotifyPreDebit(ctx, pd("D1")); err != nil {
		t.Fatal(err)
	}
	if len(f.calls("pre_debit_SI")) != 0 {
		t.Error("a pre-debit was sent for an e-NACH mandate")
	}
}

// The mode of a mandate that was never resolved from its registration is found
// by asking the status commands.
func TestPreDebitFindsTheModeOfAnUnknownMandate(t *testing.T) {
	f := &fake{cmds: map[string]string{"upi_mandate_status": `{"status":"active","authpayuid":"20000064468","amount":"1500.00"}`,
		"pre_debit_SI": `{"status":1,"action":"MANDATE_PRE_DEBIT","message":"Request Processed Successfully"}`}}
	m := mandateConn(t, f)
	err := m.NotifyPreDebit(context.Background(), connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "20000064468"}, ProviderRef: "D1", Amount: 100, Currency: "INR", DebitDate: siNow.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if c := f.calls("pre_debit_SI"); len(c) != 1 || c[0]["mandateSeqNo"] != float64(2) {
		t.Errorf("var1: %v", c)
	}
}

// The error answers the guide lists for pre_debit_SI.
func TestPreDebitAnswers(t *testing.T) {
	ctx := context.Background()
	req := connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "20000064468"}, ProviderRef: "D1", Amount: 100, Currency: "INR", DebitDate: siNow.UnixMilli()}
	for name, c := range map[string]struct {
		body string
		ok   bool
		temp bool
	}{
		"duplicate notice":      {`{"status":"E9254","message":"Pre-debit already sent for this sequence number","action":"MANDATE_PRE_DEBIT"}`, true, false},
		"execution exists":      {`{"status":"E9256","message":"Execution already sent for this sequence number","action":"MANDATE_PRE_DEBIT"}`, true, false},
		"too far ahead":         {`{"status":"E9260","message":"Pre-debit can be sent a maximum of 30 days in advance","action":"MANDATE_PRE_DEBIT"}`, false, true},
		"incorrect time period": {`{"status":"E9263","message":"Pre-debit sent during an incorrect period","action":"MANDATE_PRE_DEBIT"}`, false, true},
		"mandate not active":    {`{"status":0,"message":"Mandate is not active","action":"MANDATE_PRE_DEBIT"}`, false, false},
		"bank declined":         {`{"status":"QC","message":"Declined by the bank","action":"MANDATE_PRE_DEBIT"}`, false, false},
	} {
		f := &fake{cmds: map[string]string{"check_mandate_status": `{"status":"active","authpayuid":"20000064468","amount":1500}`, "pre_debit_SI": c.body}}
		err := mandateConn(t, f).NotifyPreDebit(ctx, req)
		if (err == nil) != c.ok || errors.Is(err, connector.ErrTemporary) != c.temp {
			t.Errorf("%s: err = %v", name, err)
		}
		// A refusal that is not temporary is the provider saying "not allowed".
		if !c.ok && !c.temp && !errors.Is(err, connector.ErrRejected) {
			t.Errorf("%s: a refused notice must be ErrRejected: %v", name, err)
		}
	}
}

func TestChargeVar1IsPerMode(t *testing.T) {
	ctx := context.Background()
	req := connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "20000064468"}, ProviderRef: "D1", Amount: 49900, Currency: "INR"}
	okDebit := `{"status":1,"message":"Transaction Processed successfully","details":{"D1":{"authpayuid":"20000064468","transactionid":"D1","amount":"499.00","payuid":"25600342065","status":"captured","field9":"Transaction Completed Successfully"}}}`

	// UPI: the sequence number of the notice for this debit.
	f := &fake{cmds: map[string]string{"upi_mandate_status": `{"status":"active","authpayuid":"20000064468","amount":"1500.00"}`, "si_transaction": okDebit,
		"pre_debit_SI": `{"status":1,"action":"MANDATE_PRE_DEBIT","message":"ok"}`}}
	m := mandateConn(t, f)
	if err := m.NotifyPreDebit(ctx, connector.PreDebitRequest{Mandate: req.Mandate, ProviderRef: "D1", Amount: 49900, Currency: "INR", DebitDate: siNow.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	s, err := m.ChargeMandate(ctx, req)
	if err != nil || s.Status != connector.PaymentSucceeded || s.Method != "upi" {
		t.Fatalf("%+v %v", s, err)
	}
	if c := f.calls("si_transaction"); len(c) != 1 || c[0]["mandateSeqNo"] != float64(2) || c[0]["invoiceDisplayNumber"] != "D1" {
		t.Errorf("UPI var1: %v", c)
	}

	// Net banking: no invoice number either, and the method is reported.
	f = &fake{cmds: map[string]string{"NB_mandate_status": `{"status":"SUCCESS","authpayuid":"20000064468","amount":"1500.00"}`, "si_transaction": okDebit}}
	m = mandateConn(t, f)
	s, err = m.ChargeMandate(ctx, req)
	if err != nil || s.Method != "emandate" {
		t.Fatalf("%+v %v", s, err)
	}
	if c := f.calls("si_transaction"); len(c) != 1 || c[0]["invoiceDisplayNumber"] != nil || c[0]["mandateSeqNo"] != nil {
		t.Errorf("e-NACH var1: %v", c)
	}
}

// The statuses of si_transaction's details (the guide's "Transaction status
// values"): captured is a success; pending and in progress are not final; failed
// and a blank status are failures.
func TestChargeStatuses(t *testing.T) {
	ctx := context.Background()
	req := connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "20000064468"}, ProviderRef: "D1", Amount: 49900, Currency: "INR"}
	for status, want := range map[string]connector.PaymentStatus{
		"captured": connector.PaymentSucceeded, "pending": connector.PaymentProcessing, "in progress": connector.PaymentProcessing, "in-progress": connector.PaymentProcessing,
		"failed": connector.PaymentFailed, "": connector.PaymentFailed,
	} {
		f := &fake{cmds: map[string]string{"check_mandate_status": `{"status":"active","authpayuid":"20000064468","amount":1500}`,
			"si_transaction": `{"status":1,"message":"Transaction Processed successfully","details":{"D1":{"authpayuid":"20000064468","transactionid":"D1","amount":"499.00","payuid":"","status":"` + status + `","field9":"x"}}}`}}
		s, err := mandateConn(t, f).ChargeMandate(ctx, req)
		if err != nil || s.Status != want {
			t.Errorf("%q: %+v %v, want %s", status, s, err, want)
		}
	}
}

// Each mode has its own cancel command; net-banking cancellation is offline and
// only acknowledged; an already inactive mandate is a revoked one.
func TestRevokeCommandPerMode(t *testing.T) {
	ctx := context.Background()
	ref := connector.MandateRef{ProviderMandateID: "20000064468"}
	cases := []struct {
		name, statusCmd, statusBody, revokeCmd, answer string
		ok                                             bool
	}{
		{"upi", "upi_mandate_status", `{"status":"active","authpayuid":"20000064468","amount":"1500.00"}`, "upi_mandate_revoke", `{"status":1,"action":"MANDATE_REVOKE","message":"Mandate Revoke request processed successfully"}`, true},
		{"card", "check_mandate_status", `{"status":"active","authpayuid":"20000064468","amount":1500}`, "mandate_revoke", `{"status":1,"message":"Mandate Revoked Successfully","action":"MANDATE_REVOKE"}`, true},
		{"emandate", "NB_mandate_status", `{"status":"SUCCESS","authpayuid":"20000064468","amount":"1500.00"}`, "mandate_revoke", `{"action":"MANDATE_REVOKE","status":1,"Message":"Mandate Cancel Initiated","authpayuid":20000064468}`, true},
		{"already revoked", "check_mandate_status", `{"status":"active","authpayuid":"20000064468","amount":1500}`, "mandate_revoke", `{"status":0,"message":"Mandate already revoked","action":"MANDATE_REVOKE"}`, true},
		{"not active", "upi_mandate_status", `{"status":"active","authpayuid":"20000064468","amount":"1500.00"}`, "upi_mandate_revoke", `{"status":0,"action":"MANDATE_REVOKE","message":"Mandate is not active"}`, true},
		{"refused", "check_mandate_status", `{"status":"active","authpayuid":"20000064468","amount":1500}`, "mandate_revoke", `{"status":0,"message":"Mandate not in appropriate state to perform action","action":"MANDATE_REVOKE"}`, false},
	}
	for _, c := range cases {
		f := &fake{cmds: map[string]string{c.statusCmd: c.statusBody, c.revokeCmd: c.answer}}
		err := mandateConn(t, f).RevokeMandate(ctx, ref)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, connector.ErrRejected) {
			t.Errorf("%s: a refused cancellation must be ErrRejected: %v", c.name, err)
		}
		if len(f.calls(c.revokeCmd)) != 1 {
			t.Errorf("%s: commands %v, want %s", c.name, f.commands(), c.revokeCmd)
		}
		if !strings.Contains(c.revokeCmd, "upi") && len(f.calls("upi_mandate_revoke")) != 0 {
			t.Errorf("%s: a UPI cancel was sent", c.name)
		}
	}
}
