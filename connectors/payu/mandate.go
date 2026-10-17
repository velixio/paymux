package payu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Standing Instructions (SI): PayU's recurring payments for cards, UPI
// AutoPay and net-banking e-mandates. Paymux runs the schedule and triggers
// every debit itself (merchant_triggered).
//
// TODO(spec): none of this could be checked against a PayU sandbox. The
// places to verify first, all isolated in this file: the SI hash layout
// (si_details between the udf block and the salt), the `pg` codes per method,
// the command names pre_debit_SI / si_transaction / check_mandate_status /
// mandate_revoke, and the shape of their JSON var1 and responses.

// setupAmount is what the mandate registration debits (Rs 1, spec 6.8: "may be
// Rs 0 or Rs 1 per provider"); the first real charge is a normal debit.
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
// form_post like a normal checkout) for a Rs 1 first debit that authorises the
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
	setup, _ := money.FormatDecimal(setupAmount, "INR")
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
	case "revoked", "cancelled", "canceled", "deleted", "stopped":
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
		return connector.MandateSnapshot{ProviderMandateID: string(d.MihPayID), Status: st, RawStatus: string(d.Status), Currency: "INR"}, nil
	}
	v1, _ := json.Marshal(struct {
		AuthPayuID string `json:"authPayuId"`
	}{id})
	var out struct {
		Status flexStr `json:"status"`
		Msg    string  `json:"msg"`
		Result struct {
			MandateStatus flexStr `json:"mandateStatus"`
		} `json:"result"`
	}
	if err := p.api.call(ctx, "check_mandate_status", string(v1), nil, &out); err != nil {
		return connector.MandateSnapshot{}, err
	}
	if out.Status != "1" {
		if err := checkCredentials(out.Msg); err != nil {
			return connector.MandateSnapshot{}, err
		}
		return connector.MandateSnapshot{}, connector.ErrNotFound
	}
	raw := string(out.Result.MandateStatus)
	return connector.MandateSnapshot{ProviderMandateID: id, Status: mandateStatus(raw), RawStatus: raw, Currency: "INR"}, nil
}

type preDebit struct {
	AuthPayuID    string `json:"authPayuId"`
	RequestID     string `json:"requestId"`
	DebitDate     string `json:"debitDate"`
	InvoiceAmount string `json:"invoiceAmount"`
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
		DebitDate: istDate(req.DebitDate), InvoiceAmount: amt})
	var out struct {
		Status flexStr `json:"status"`
		Msg    string  `json:"msg"`
	}
	if err := p.api.call(ctx, "pre_debit_SI", string(v1), nil, &out); err != nil {
		return err
	}
	if out.Status == "1" || strings.Contains(strings.ToLower(out.Msg), "already") {
		return nil // a repeated notice for the same debit is fine
	}
	if cerr := checkCredentials(out.Msg); cerr != nil {
		return cerr
	}
	return fmt.Errorf("payu: pre-debit notice refused: %s", out.Msg)
}

type siCharge struct {
	AuthPayuID    string `json:"authPayuId"`
	InvoiceAmount string `json:"invoiceAmount"`
	TransactionID string `json:"transactionId"`
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
	v1, _ := json.Marshal(siCharge{AuthPayuID: req.Mandate.ProviderMandateID, InvoiceAmount: amt, TransactionID: req.ProviderRef})
	var out struct {
		Status  flexStr              `json:"status"`
		Msg     string               `json:"msg"`
		Details map[string]txnDetail `json:"transaction_details"`
	}
	if err := p.api.call(ctx, "si_transaction", string(v1), nil, &out); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	low := strings.ToLower(out.Msg)
	switch {
	case out.Status == "1":
		if d, ok := out.Details[req.ProviderRef]; ok {
			return d.snapshot(req.ProviderRef)
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
	v1, _ := json.Marshal(struct {
		AuthPayuID string `json:"authPayuId"`
	}{ref.ProviderMandateID})
	var out struct {
		Status flexStr `json:"status"`
		Msg    string  `json:"msg"`
	}
	if err := p.api.call(ctx, "mandate_revoke", string(v1), nil, &out); err != nil {
		return err
	}
	if out.Status == "1" || strings.Contains(strings.ToLower(out.Msg), "already") {
		return nil
	}
	if cerr := checkCredentials(out.Msg); cerr != nil {
		return cerr
	}
	if strings.Contains(strings.ToLower(out.Msg), "not found") {
		return connector.ErrNotFound
	}
	return fmt.Errorf("payu: revoking mandate: %s", out.Msg)
}

var _ connector.MandateManager = (*payu)(nil)
