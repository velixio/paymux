package payu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Standing Instructions (SI): PayU's recurring payments for cards, UPI
// AutoPay and net-banking e-mandates. Paymux runs the schedule and triggers
// every debit itself (merchant_triggered).
//
// Verified against the PayU test environment (2026-10-18): the SI registration
// form (PayU showed the plan: maximum mandate amount, "As Presented", start and
// end date; a card was registered and the 3-D Secure simulator completed), the
// status commands (check_mandate_status for cards answers with the status as
// the "status" string and the amount as a number; upi_mandate_status and
// NB_mandate_status exist for the other modes), pre_debit_SI and si_transaction
// with PayU's documented var1 (authpayuid in lower case, invoiceDisplayNumber,
// amount, txnid; answered "failed" with field9 "Invalid/missing card token" for
// this merchant, which has no card token), and mandate_revoke (status 1,
// "Mandate Revoked Successfully"; the mandate then reads "deleted").
//
// TODO(spec): not seen with a registered UPI or net-banking mandate (the
// pg codes and the UPI and net-banking status answers are from the
// documentation), nor a captured debit, nor the pre-debit rule that the debit
// may follow only 48 hours after the notice (PayU's documentation; Paymux's
// notice lead is configurable).

// setupAmount (Rs 1) is the verification amount a registration debits when
// the request names none (Info.MandateSetupAmount). A registration for a
// running plan carries the first period's amount instead, as the single AFA
// transaction (RBI e-mandate framework).
const setupAmount int64 = 100

// siDetails is the si_details form field (JSON). Field order is part of the
// signed string, so it is a struct, not a map.
type siDetails struct {
	BillingAmount    string `json:"billingAmount"`
	BillingCurrency  string `json:"billingCurrency"`
	BillingCycle     string `json:"billingCycle"`
	BillingInterval  int    `json:"billingInterval"`
	PaymentStartDate string `json:"paymentStartDate"`
	PaymentEndDate   string `json:"paymentEndDate"`
}

var pgCodes = map[string]string{"upi": "UPI", "card": "CC", "emandate": "NB"}

// siHash is sha512(key|txnid|amount|productinfo|firstname|email|udf1..udf5||||||si_details|SALT).
func siHash(key, salt, txnid, amount, productinfo, firstname, email string, udf [5]string, si string) string {
	return sha512Hex(key, txnid, amount, productinfo, firstname, email,
		udf[0], udf[1], udf[2], udf[3], udf[4], "", "", "", "", "", si, salt)
}

func istDate(ms int64) string { return time.UnixMilli(ms).In(ist).Format("2006-01-02") }

// CreateMandateCheckout builds the SI registration form (an auto-submitting
// form_post like a normal checkout) whose first debit (Amount) authorises the
// mandate up to MaxAmount per debit. The registration's mihpayid becomes the
// mandate id (authPayuId) used by every later call.
func (p *payu) CreateMandateCheckout(_ context.Context, req connector.MandateRequest) (connector.CheckoutResult, error) {
	if req.Currency != "INR" {
		return connector.CheckoutResult{}, fmt.Errorf("payu: currency %q unsupported (INR only)", req.Currency)
	}
	pg, ok := pgCodes[req.Method]
	if !ok {
		return connector.CheckoutResult{}, fmt.Errorf("payu: mandate method %q unsupported", req.Method)
	}
	if req.ProviderRef == "" || req.MaxAmount <= 0 {
		return connector.CheckoutResult{}, errors.New("payu: provider_ref and a positive max amount are required")
	}
	maxStr, err := money.FormatDecimal(req.MaxAmount, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	first := req.Amount
	if first <= 0 {
		first = setupAmount
	}
	setup, err := money.FormatDecimal(first, "INR")
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	end := req.ValidUntil
	if end == 0 {
		end = p.deps.Now().AddDate(5, 0, 0).UnixMilli()
	}
	siJSON, _ := json.Marshal(siDetails{BillingAmount: maxStr, BillingCurrency: "INR", BillingCycle: "ADHOC", BillingInterval: 1,
		PaymentStartDate: istDate(p.deps.Now().UnixMilli()), PaymentEndDate: istDate(end)})
	productinfo := "Subscription mandate"
	firstname := orDefault(req.Customer.Name, "Customer")
	email := orDefault(req.Customer.Email, "noreply@example.com")
	ret := p.deps.URLs.ReturnURL(req.ProviderRef)
	udf := [5]string{req.MandateID}
	fields := map[string]string{
		"key": p.cfg.Key, "txnid": req.ProviderRef, "amount": setup, "productinfo": productinfo, "firstname": firstname,
		"email": email, "phone": req.Customer.Phone, "surl": ret, "furl": ret, "udf1": udf[0],
		"service_provider": "payu_paisa", "si": "1", "api_version": "7", "si_details": string(siJSON), "pg": pg,
		"hash": siHash(p.cfg.Key, p.cfg.Salt, req.ProviderRef, setup, productinfo, firstname, email, udf, string(siJSON)),
	}
	return connector.CheckoutResult{NextAction: connector.NextAction{Kind: connector.ActionFormPost, URL: p.api.paymentURL(), Fields: fields}}, nil
}

// mandateStatus maps PayU's mandate states.
func mandateStatus(raw string) string {
	switch strings.ToLower(raw) {
	case "active", "success", "authorized", "captured":
		return "active"
	case "revoked", "cancelled", "canceled", "deleted", "discarded", "stopped":
		return "revoked"
	case "expired", "completed":
		return "expired"
	case "failure", "failed", "rejected":
		return "failed"
	}
	return "pending" // "pending", "initiated", "paused" and anything new
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// FetchMandate reads a mandate. Right after the customer returns only the
// registration reference (the txnid) is known: verify_payment turns it into
// the mihpayid, which then is the mandate id. Later calls ask for the mandate's
// own status.
func (p *payu) FetchMandate(ctx context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	id := ref.ProviderMandateID
	if id == "" {
		return connector.MandateSnapshot{}, errors.New("payu: MandateRef has no id")
	}
	if !allDigits(id) {
		var out struct {
			Status  flexStr              `json:"status"`
			Msg     string               `json:"msg"`
			Details map[string]txnDetail `json:"transaction_details"`
		}
		if err := p.api.call(ctx, "verify_payment", id, nil, &out); err != nil {
			return connector.MandateSnapshot{}, err
		}
		d, ok := out.Details[id]
		if !ok {
			if err := checkCredentials(out.Msg); err != nil {
				return connector.MandateSnapshot{}, err
			}
			return connector.MandateSnapshot{}, connector.ErrNotFound
		}
		st := mandateStatus(string(d.Status))
		if st == "revoked" || st == "expired" {
			st = "failed" // a registration that did not succeed
		}
		snap := connector.MandateSnapshot{ProviderMandateID: string(d.MihPayID), Status: st, RawStatus: string(d.Status), Currency: "INR"}
		if st == "active" {
			// The registration transaction is the first period's payment.
			ps, err := d.snapshot(id)
			if err != nil {
				return connector.MandateSnapshot{}, err
			}
			snap.SetupPayment = &ps
		}
		return snap, nil
	}
	// The mandate's own status. Each payment mode has its own command: cards
	// check_mandate_status, UPI upi_mandate_status, net banking NB_mandate_status.
	// Every one answers "not found" for a mandate of another mode, so they are
	// asked in turn.
	for _, cmd := range []string{"check_mandate_status", "upi_mandate_status", "NB_mandate_status"} {
		v1, _ := json.Marshal(mandateCall{AuthPayuID: id, RequestID: p.requestID()})
		var out struct {
			Status  flexStr `json:"status"`
			Msg     string  `json:"msg"`
			Message string  `json:"message"`
			Amount  flexStr `json:"amount"`
			End     string  `json:"mandateEndDate"`
			Method  string  `json:"-"`
		}
		if err := p.api.call(ctx, cmd, string(v1), nil, &out); err != nil {
			return connector.MandateSnapshot{}, err
		}
		if err := checkCredentials(firstNonEmpty(out.Msg, out.Message)); err != nil {
			return connector.MandateSnapshot{}, err
		}
		// A mandate that exists answers with its status as the "status" string
		// ("active", "pending", "cancelled", "deleted"...); a refusal has a number.
		if _, err := strconv.Atoi(string(out.Status)); err == nil || out.Status == "" {
			continue
		}
		snap := connector.MandateSnapshot{ProviderMandateID: id, Status: mandateStatus(string(out.Status)), RawStatus: string(out.Status), Currency: "INR",
			Method: map[string]string{"check_mandate_status": "card", "upi_mandate_status": "upi", "NB_mandate_status": "emandate"}[cmd]}
		if amt, err := money.ParseDecimal(string(out.Amount), "INR"); err == nil {
			snap.MaxAmount = amt
		}
		if t, err := time.ParseInLocation("2006-01-02", out.End, ist); err == nil {
			snap.ValidUntil = t.UnixMilli()
		}
		return snap, nil
	}
	return connector.MandateSnapshot{}, connector.ErrNotFound
}

// preDebit is the var1 of pre_debit_SI (PayU's documentation: authpayuid,
// requestId, amount, debitDate, invoiceDisplayNumber).
type preDebit struct {
	AuthPayuID           string `json:"authpayuid"`
	RequestID            string `json:"requestId"`
	Amount               string `json:"amount"`
	DebitDate            string `json:"debitDate"`
	InvoiceDisplayNumber string `json:"invoiceDisplayNumber"`
}

// NotifyPreDebit sends the pre-debit notification (pre_debit_SI): PayU
// informs the customer; RBI requires it at least 24h before every debit.
// ProviderRef is the txnid of the debit that will follow.
func (p *payu) NotifyPreDebit(ctx context.Context, req connector.PreDebitRequest) error {
	if req.Currency != "INR" {
		return fmt.Errorf("payu: currency %q unsupported (INR only)", req.Currency)
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return err
	}
	v1, _ := json.Marshal(preDebit{AuthPayuID: req.Mandate.ProviderMandateID, RequestID: req.ProviderRef,
		Amount: amt, DebitDate: istDate(req.DebitDate), InvoiceDisplayNumber: req.ProviderRef})
	var out struct {
		Status  flexStr `json:"status"`
		Msg     string  `json:"msg"`
		Message string  `json:"message"`
	}
	if err := p.api.call(ctx, "pre_debit_SI", string(v1), nil, &out); err != nil {
		return err
	}
	msg := firstNonEmpty(out.Msg, out.Message)
	if out.Status == "1" || strings.Contains(strings.ToLower(msg), "already") {
		return nil // a repeated notice for the same debit is fine
	}
	if cerr := checkCredentials(msg); cerr != nil {
		return cerr
	}
	return fmt.Errorf("payu: pre-debit notice refused: %s", msg)
}

// siCharge is the var1 of si_transaction (the field names are from PayU's
// documentation and were accepted by the test environment; the first guess,
// authPayuId / invoiceAmount / transactionId, was refused with "Mandatory column
// missing").
type siCharge struct {
	AuthPayuID           string `json:"authpayuid"`
	InvoiceDisplayNumber string `json:"invoiceDisplayNumber"`
	Amount               string `json:"amount"`
	TxnID                string `json:"txnid"`
}

// ChargeMandate debits the mandate (si_transaction). ProviderRef is the
// transactionId, which PayU refuses to take twice: a repeated call therefore
// answers with the current state of the first debit (verify_payment) instead
// of debiting again. A declined debit is a failed snapshot, not an error.
func (p *payu) ChargeMandate(ctx context.Context, req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	if req.Currency != "INR" {
		return connector.PaymentSnapshot{}, fmt.Errorf("payu: currency %q unsupported (INR only)", req.Currency)
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil || req.Amount <= 0 || req.ProviderRef == "" {
		return connector.PaymentSnapshot{}, errors.New("payu: charge needs a positive amount and a provider_ref")
	}
	v1, _ := json.Marshal(siCharge{AuthPayuID: req.Mandate.ProviderMandateID, InvoiceDisplayNumber: req.ProviderRef, Amount: amt, TxnID: req.ProviderRef})
	var out struct {
		Status  flexStr `json:"status"`
		Msg     string  `json:"msg"`
		Message string  `json:"message"`
		// The answer to a debit it processed: status 1 and, per transaction, its
		// own outcome ("captured" or "failed" with field9 saying why).
		Details map[string]siDetail `json:"details"`
	}
	if err := p.api.call(ctx, "si_transaction", string(v1), nil, &out); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	out.Msg = firstNonEmpty(out.Msg, out.Message)
	low := strings.ToLower(out.Msg)
	switch {
	case out.Status == "1":
		if d, ok := out.Details[req.ProviderRef]; ok {
			return d.snapshot(req.ProviderRef, req.Amount)
		}
		// Accepted for processing; the outcome arrives by webhook or verify_payment.
		return connector.PaymentSnapshot{ProviderRef: req.ProviderRef, Status: connector.PaymentProcessing, RawStatus: "pending",
			Amount: req.Amount, Currency: "INR"}, nil
	case strings.Contains(low, "duplicate") || strings.Contains(low, "already"):
		return p.FetchPayment(ctx, connector.PaymentRef{ProviderRef: req.ProviderRef})
	case strings.Contains(low, "exceed") || strings.Contains(low, "limit"):
		return connector.PaymentSnapshot{}, connector.ErrMandateLimit
	}
	if cerr := checkCredentials(out.Msg); cerr != nil {
		return connector.PaymentSnapshot{}, cerr
	}
	return connector.PaymentSnapshot{ProviderRef: req.ProviderRef, Status: connector.PaymentFailed, RawStatus: "failure",
		Amount: req.Amount, Currency: "INR", ErrorCode: "si_rejected", ErrorMessage: out.Msg}, nil
}

// RevokeMandate cancels the mandate at PayU (mandate_revoke).
func (p *payu) RevokeMandate(ctx context.Context, ref connector.MandateRef) error {
	v1, _ := json.Marshal(mandateCall{AuthPayuID: ref.ProviderMandateID, RequestID: p.requestID()})
	var out struct {
		Status  flexStr `json:"status"`
		Msg     string  `json:"msg"`
		Message string  `json:"message"`
	}
	if err := p.api.call(ctx, "mandate_revoke", string(v1), nil, &out); err != nil {
		return err
	}
	msg := firstNonEmpty(out.Msg, out.Message)
	if out.Status == "1" || strings.Contains(strings.ToLower(msg), "already") {
		return nil
	}
	if cerr := checkCredentials(msg); cerr != nil {
		return cerr
	}
	if strings.Contains(strings.ToLower(msg), "not found") {
		return connector.ErrNotFound
	}
	return fmt.Errorf("payu: revoking mandate: %s", msg)
}

// mandateCall is the var1 of check_mandate_status and mandate_revoke. PayU
// refuses them without a requestId ("requestId is mandatory", verified live).
type mandateCall struct {
	AuthPayuID string `json:"authpayuid"`
	RequestID  string `json:"requestId"`
}

// requestID is a fresh id for a mandate call: the clock plus a counter.
func (p *payu) requestID() string {
	n := p.seq.Add(1)
	return "pmx" + strconv.FormatInt(p.deps.Now().UnixNano(), 36) + strconv.FormatUint(n, 36)
}

var _ connector.MandateManager = (*payu)(nil)

// siDetail is one debit in the answer of si_transaction.
type siDetail struct {
	PayUID        flexStr `json:"payuid"`
	TransactionID flexStr `json:"transactionid"`
	Amount        flexStr `json:"amount"`
	Status        flexStr `json:"status"`
	Field9        flexStr `json:"field9"`
}

func (d siDetail) snapshot(ref string, fallbackAmount int64) (connector.PaymentSnapshot, error) {
	amt := fallbackAmount
	if v, err := money.ParseDecimal(string(d.Amount), "INR"); err == nil && v > 0 {
		amt = v
	}
	snap := connector.PaymentSnapshot{ProviderRef: ref, ProviderPaymentID: string(d.PayUID), Status: mapStatus(string(d.Status)), RawStatus: string(d.Status),
		Amount: amt, Currency: "INR", Method: "card"}
	if snap.Status == connector.PaymentFailed || snap.Status == connector.PaymentCanceled {
		snap.ErrorCode, snap.ErrorMessage = "si_rejected", string(d.Field9)
	}
	return snap, nil
}
