package payu

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// The testdata files verify_*, refund_*, action_* and command_* are responses
// recorded from the PayU test environment (test.payu.in), with the txnid and the
// refund token replaced by the test's own. They pin the real shapes.

func body(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRealVerifyPaymentCard(t *testing.T) {
	f := &fake{verify: body(t, "verify_success_card")}
	snap, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.PaymentSucceeded || snap.Amount != 10050 || snap.ProviderPaymentID != "20000063887" || snap.CreatedAt == 0 {
		t.Errorf("%+v", snap)
	}
	// PayU sends error_code E000 / "NO ERROR" on success: not an error to report.
	if snap.ErrorCode != "" || snap.ErrorMessage != "" {
		t.Errorf("a successful payment carries no error: %q %q", snap.ErrorCode, snap.ErrorMessage)
	}
	// PayU's modes are CC, DC, NB, UPI, WALLET...; Paymux names methods card, netbanking, upi, wallet.
	if snap.Method != "card" {
		t.Errorf("method = %q", snap.Method)
	}
}

func TestRealVerifyPaymentPending(t *testing.T) {
	f := &fake{verify: body(t, "verify_pending_netbanking")}
	snap, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentProcessing || snap.Method != "netbanking" || snap.Amount != 10050 {
		t.Errorf("%+v %v", snap, err)
	}
}

// An unknown txnid is answered with 200 and the txnid echoed back with status
// "Not Found": PayU cannot tell an unknown id from a checkout the customer has
// not submitted yet, and Paymux only asks about txnids it issued, so this is a
// payment still waiting for the customer.
func TestRealVerifyPaymentNotFound(t *testing.T) {
	f := &fake{verify: body(t, "verify_not_found")}
	snap, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.ProviderPaymentID != "Not Found" && snap.ProviderPaymentID != "" {
		t.Errorf("%+v %v", snap, err)
	}
	if snap.ProviderPaymentID != "" {
		t.Errorf("the placeholder %q must not become a payment id", snap.ProviderPaymentID)
	}
}

func TestRealInvalidHashIsInvalidCredentials(t *testing.T) {
	f := &fake{verify: body(t, "command_invalid_hash"), refund: body(t, "command_invalid_hash")}
	c := newConn(t, f)
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("verify: %v", err)
	}
	if _, err := c.(connector.Refunder).Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf1", ProviderPaymentID: "1", Amount: 100}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("refund: %v", err)
	}
}

// get_Transaction_Details names things differently from verify_payment: the
// PayU id is "id", the status is the unmapped one ("captured"), and the fee and
// the tax on it are mer_service_fee and mer_service_tax.
func TestRealListPayments(t *testing.T) {
	f := &fake{listing: body(t, "listing_captured")}
	tr := connector.TimeRange{From: 1792272000000, To: 1792358400000} // 2026-10-18 IST
	pg, err := newConn(t, f).(connector.Lister).ListPayments(context.Background(), tr, "")
	// The fixture also holds the row PayU adds for a refund of the payment: the same
	// id, txnid, amount and status "captured" as the payment, told apart only by its
	// request_id. It is not a second payment.
	if err != nil || len(pg.Items) != 1 {
		t.Fatalf("%+v %v", pg, err)
	}
	p := pg.Items[0]
	if p.ProviderRef != "pay_ref_01" || p.ProviderPaymentID != "20000063902" || p.Status != connector.PaymentSucceeded || p.Amount != 10050 ||
		p.Fee != 201 || p.Tax != 36 || p.Method != "card" || p.ErrorCode != "" {
		t.Errorf("%+v", p)
	}
}

func TestStatusMapUnmapped(t *testing.T) {
	for raw, want := range map[string]connector.PaymentStatus{
		"captured": connector.PaymentSucceeded, "auth": connector.PaymentAuthorized, "bounced": connector.PaymentFailed, "dropped": connector.PaymentCanceled,
	} {
		if got := mapStatus(raw); got != want {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
}

func TestRealRefundQueued(t *testing.T) {
	f := &fake{refund: body(t, "refund_queued")}
	snap, err := newConn(t, f).(connector.Refunder).Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_ref_01", ProviderPaymentID: "20000063887", Amount: 1025, Currency: "INR"})
	if err != nil || snap.Status != connector.RefundPending || snap.ProviderRefundID != "900000000000413566" || snap.ProviderRef != "rf_ref_01" {
		t.Errorf("%+v %v", snap, err)
	}
	if got := f.last(); got.Get("var3") != "10.25" {
		t.Errorf("amount sent as %q", got.Get("var3"))
	}
}

func TestRealRefundOverAmountIsNotRefundable(t *testing.T) {
	f := &fake{refund: body(t, "refund_invalid_amount")}
	_, err := newConn(t, f).(connector.Refunder).Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_ref_02", ProviderPaymentID: "20000063887", Amount: 999900, Currency: "INR"})
	var nr connector.ErrNotRefundable
	if !errors.As(err, &nr) || nr.Reason == "" {
		t.Errorf("%v", err)
	}
}

// Sending a refund token again fails with error_code 227 ("same amount and same
// token not allowed"), which is how PayU deduplicates. A retried job must get the
// refund it already made: it is looked up among the payment's actions by token.
func TestRealRefundDuplicateTokenAdoptsTheExistingRefund(t *testing.T) {
	f := &fake{refund: body(t, "refund_duplicate_token"), check: body(t, "action_by_payment")}
	c := newConn(t, f).(connector.Refunder)
	req := connector.RefundRequest{ProviderRef: "rf_ref_01", ProviderPaymentID: "20000063887", Amount: 1025, Currency: "INR"}
	snap, err := c.Refund(context.Background(), req)
	if err != nil || snap.ProviderRefundID != "900000000000413566" || snap.Amount != 1025 || snap.Status != connector.RefundPending || snap.ProviderRef != "rf_ref_01" {
		t.Fatalf("%+v %v", snap, err)
	}
	if got := f.last(); got.Get("command") != "check_action_status" || got.Get("var1") != "20000063887" || got.Get("var2") != "payuid" {
		t.Errorf("the payment's actions are listed with var2=payuid: %v", got)
	}
	req.Amount = 5000
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(context.Background(), req); err == nil || errors.As(err, &nr) {
		t.Errorf("a token reused with another amount is a conflict, not an adoption: %v", err)
	}
}

func TestRealFetchRefundByRequestID(t *testing.T) {
	f := &fake{check: body(t, "action_by_request")}
	c := newConn(t, f).(connector.Refunder)
	s, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "900000000000413566"})
	if err != nil || s.Status != connector.RefundPending || s.RawStatus != "queued" || s.Amount != 1025 || s.ProviderRef != "rf_ref_01" || s.ProviderPaymentID != "20000063887" {
		t.Fatalf("%+v %v", s, err)
	}
	if got := f.last(); got.Get("var1") != "900000000000413566" || got.Get("var2") != "" {
		t.Errorf("%v", got)
	}
	// The entry must be the one asked for.
	if _, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "900000000000000001"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("another request id: %v", err)
	}
}

// By token (the Paymux refund reference) and the payment: the payment's actions
// include the capture, which is not a refund and must never be returned.
func TestRealFetchRefundByTokenAmongTheActionsOfAPayment(t *testing.T) {
	f := &fake{check: body(t, "action_by_payment")}
	c := newConn(t, f).(connector.Refunder)
	s, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRef: "rf_ref_01", ProviderPaymentID: "20000063887"})
	if err != nil || s.ProviderRefundID != "900000000000413566" || s.Amount != 1025 {
		t.Fatalf("%+v %v", s, err)
	}
	if got := f.last(); got.Get("var1") != "20000063887" || got.Get("var2") != "payuid" {
		t.Errorf("%v", got)
	}
	if _, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRef: "rf_other", ProviderPaymentID: "20000063887"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("a token that matches no refund (the capture must not stand in): %v", err)
	}
	if _, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRef: "rf_ref_01"}); err == nil {
		t.Error("a token without its payment cannot be looked up")
	}
}

func TestRealFetchRefundUnknownRequestID(t *testing.T) {
	f := &fake{check: body(t, "action_unknown")}
	if _, err := newConn(t, f).(connector.Refunder).FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "900000000000999999"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("%v", err)
	}
}
