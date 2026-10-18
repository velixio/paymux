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
// Read against PayU's recurring-payments guide and run on the test environment
// (2026-10-18; docs/provider-verification.md has the list of what differed and
// what could not be run): the SI registration form (the hosted page shows the
// plan; cards are captured with 3-D Secure), the status commands, mandate_revoke
// and upi_mandate_revoke, and the request shapes of pre_debit_SI and
// si_transaction.
//
// TODO(spec): the test merchant cannot register a mandate (a card payment comes
// back with payment_source "payu", not "sist", and the mandate stays pending; UPI
// and e-NACH are not available to it), so no registered mandate, no accepted
// pre-debit and no captured debit has been seen, and neither the UPI nor the
// net-banking status answers nor the 48-hour notice rule on the test environment.

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

// pgCodes are the pg values of the guide's three consent transactions: cards
// CC (or DC), UPI, and ENACH for a net-banking e-mandate (not NB).
var pgCodes = map[string]string{"upi": "UPI", "card": "CC", "emandate": "ENACH"}

// The three kinds of mandate, as Info.MandateLimits and MandateSnapshot name them.
const (
	methodCard     = "card"
	methodUPI      = "upi"
	methodEMandate = "emandate"
)

// statusCommands are the postservice commands that read a mandate, by method.
var statusCommands = map[string]string{methodCard: "check_mandate_status", methodUPI: "upi_mandate_status", methodEMandate: "NB_mandate_status"}

// mandateMethodOf names the method of a registration payment by its PayU mode.
func mandateMethodOf(mode string) string {
	switch strings.ToUpper(mode) {
	case "CC", "DC", "CARD":
		return methodCard
	case "UPI":
		return methodUPI
	case "ENACH", "NB":
		return methodEMandate
	}
	return ""
}

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
	if req.Method == methodUPI {
		fields["bankcode"] = "UPI"
	}
	return connector.CheckoutResult{NextAction: connector.NextAction{Kind: connector.ActionFormPost, URL: p.api.paymentURL(), Fields: fields}}, nil
}

// mandateStatus maps PayU's mandate states: cards (active, cancelled,
// discarded, deleted, failed, expired, paused), UPI (active, revoked, pause) and
// net banking (SUCCESS, INITIATED, FAILED and the CANCEL_* family). A cancel
// that failed leaves the mandate active; one in flight is not yet revoked.
func mandateStatus(raw string) string {
	switch strings.ToLower(raw) {
	case "active", "success", "authorized", "captured", "unpause", "cancel_failed", "cancel_initiation_failed":
		return "active"
	case "revoked", "cancelled", "canceled", "deleted", "discarded", "stopped", "cancel_success":
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
		method := mandateMethodOf(string(d.Mode))
		p.rememberMethod(ctx, string(d.MihPayID), method)
		snap := connector.MandateSnapshot{ProviderMandateID: string(d.MihPayID), Status: st, RawStatus: string(d.Status), Currency: "INR", Method: method}
		// A consent is valid only with payment_source "sist". A successful
		// payment sourced "payu" is PayU's "Mandate failed, Transaction
		// successful": the customer was charged but no mandate exists. The
		// charge is still reported so that it is refunded.
		noMandate := st == "active" && d.PaymentSource != "" && !strings.EqualFold(string(d.PaymentSource), "sist")
		if noMandate {
			st, snap.Status = "failed", "failed"
			snap.RawStatus = string(d.Status) + " (payment_source " + string(d.PaymentSource) + ")"
		}
		if st == "active" || noMandate {
			// The registration transaction is the first period's payment.
			ps, err := d.snapshot(id)
			if err != nil {
				return connector.MandateSnapshot{}, err
			}
			snap.SetupPayment = &ps
		}
		return snap, nil
	}
	return p.statusByID(ctx, id)
}

// statusByID reads the mandate's own status. Each mode has its own command
// (cards check_mandate_status, UPI upi_mandate_status, net banking
// NB_mandate_status) and each answers "not found" for a mandate of another
// mode, so the command of the known method is asked first, then the others. The
// mode that answered is remembered.
func (p *payu) statusByID(ctx context.Context, id string) (connector.MandateSnapshot, error) {
	order := []string{methodCard, methodUPI, methodEMandate}
	if known := p.knownMethod(ctx, id); known != "" {
		order = append([]string{known}, without(order, known)...)
	}
	for _, method := range order {
		cmd := statusCommands[method]
		v1, _ := json.Marshal(mandateCall{AuthPayuID: id, RequestID: p.requestID()})
		var out struct {
			Status  flexStr `json:"status"`
			Msg     string  `json:"msg"`
			Message string  `json:"message"`
			Amount  flexStr `json:"amount"`
			End     string  `json:"mandateEndDate"`
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
		p.rememberMethod(ctx, id, method)
		snap := connector.MandateSnapshot{ProviderMandateID: id, Status: mandateStatus(string(out.Status)), RawStatus: string(out.Status), Currency: "INR", Method: method}
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

func without(in []string, drop string) []string {
	var out []string
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// The method of a mandate is kept per account (connector KV) once it is known,
// because the debit commands differ by mode.
func methodKey(id string) string { return "payu:mm:" + id }

func (p *payu) rememberMethod(ctx context.Context, id, method string) {
	if method != "" && p.deps.KV != nil && id != "" {
		_ = p.deps.KV.Set(ctx, methodKey(id), []byte(method), 0)
	}
}

func (p *payu) knownMethod(ctx context.Context, id string) string {
	if p.deps.KV == nil {
		return ""
	}
	if v, ok, err := p.deps.KV.Get(ctx, methodKey(id)); err == nil && ok {
		return string(v)
	}
	return ""
}

// mandateMethod is the method of a registered mandate: remembered, or found by
// asking the status commands. "" when PayU does not know the mandate.
func (p *payu) mandateMethod(ctx context.Context, id string) (string, error) {
	if m := p.knownMethod(ctx, id); m != "" {
		return m, nil
	}
	snap, err := p.statusByID(ctx, id)
	if errors.Is(err, connector.ErrNotFound) {
		return "", nil
	}
	return snap.Method, err
}

// mandateSeq is the execution sequence number of UPI AutoPay: the registration
// is execution 1, so the first pre-debit is 2. Each debit (one ProviderRef) keeps
// the number its notice took, so a repeated notice or the debit itself reuses it.
func (p *payu) mandateSeq(ctx context.Context, id, ref string) int {
	if p.deps.KV == nil {
		return 2
	}
	refKey := "payu:mseqref:" + id + ":" + ref
	if v, ok, err := p.deps.KV.Get(ctx, refKey); err == nil && ok {
		if n, err := strconv.Atoi(string(v)); err == nil {
			return n
		}
	}
	next := 2
	if v, ok, err := p.deps.KV.Get(ctx, "payu:mseq:"+id); err == nil && ok {
		if n, err := strconv.Atoi(string(v)); err == nil {
			next = n + 1
		}
	}
	_ = p.deps.KV.Set(ctx, "payu:mseq:"+id, []byte(strconv.Itoa(next)), 0)
	_ = p.deps.KV.Set(ctx, refKey, []byte(strconv.Itoa(next)), 0)
	return next
}

// preDebit is the var1 of pre_debit_SI (PayU's documentation: authpayuid,
// requestId, amount, debitDate, invoiceDisplayNumber).
// Cards carry an invoiceDisplayNumber, UPI AutoPay a mandateSeqNo.
type preDebit struct {
	AuthPayuID           string `json:"authpayuid"`
	RequestID            string `json:"requestId"`
	Amount               string `json:"amount"`
	DebitDate            string `json:"debitDate"`
	InvoiceDisplayNumber string `json:"invoiceDisplayNumber,omitempty"`
	MandateSeqNo         int    `json:"mandateSeqNo,omitempty"`
}

// NotifyPreDebit sends the pre-debit notification (pre_debit_SI): PayU
// informs the customer; RBI requires it before every debit, and PayU wants it
// at least 48 hours ahead (24 for daily and ad-hoc UPI) and at most 30 days
// ahead. Only cards and UPI take one: a net-banking e-mandate has none.
// ProviderRef is the txnid of the debit that will follow.
func (p *payu) NotifyPreDebit(ctx context.Context, req connector.PreDebitRequest) error {
	if req.Currency != "INR" {
		return fmt.Errorf("payu: currency %q unsupported (INR only)", req.Currency)
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return err
	}
	id := req.Mandate.ProviderMandateID
	method, err := p.mandateMethod(ctx, id)
	if err != nil {
		return err
	}
	pd := preDebit{AuthPayuID: id, RequestID: req.ProviderRef, Amount: amt, DebitDate: istDate(req.DebitDate)}
	switch method {
	case methodEMandate:
		return nil
	case methodUPI:
		// The guide asks for the invoice number of cards only, but the test
		// environment refuses a notice without one ("Invoice display number not
		// present") before it looks at the mode, so UPI sends it too.
		pd.InvoiceDisplayNumber = req.ProviderRef
		pd.MandateSeqNo = p.mandateSeq(ctx, id, req.ProviderRef)
	default:
		pd.InvoiceDisplayNumber = req.ProviderRef
	}
	v1, _ := json.Marshal(pd)
	var out struct {
		Status  flexStr `json:"status"`
		Msg     string  `json:"msg"`
		Message string  `json:"message"`
	}
	if err := p.api.call(ctx, "pre_debit_SI", string(v1), nil, &out); err != nil {
		return err
	}
	msg := firstNonEmpty(out.Msg, out.Message)
	switch {
	case out.Status == "1", strings.Contains(strings.ToLower(msg), "already"):
		return nil // a repeated notice for the same debit is fine
	case out.Status == "E9254", out.Status == "E9256": // the notice or the execution of this sequence number exists
		return nil
	case out.Status == "E9260", out.Status == "E9263": // more than 30 days ahead, or at the wrong time
		return fmt.Errorf("payu: pre-debit notice not accepted now (%s): %s: %w", out.Status, msg, connector.ErrTemporary)
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
	InvoiceDisplayNumber string `json:"invoiceDisplayNumber,omitempty"`
	Amount               string `json:"amount"`
	TxnID                string `json:"txnid"`
	MandateSeqNo         int    `json:"mandateSeqNo,omitempty"`
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
	id := req.Mandate.ProviderMandateID
	method, err := p.mandateMethod(ctx, id)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	sc := siCharge{AuthPayuID: id, Amount: amt, TxnID: req.ProviderRef}
	switch method {
	case methodEMandate: // no invoice number, no sequence number
	case methodUPI:
		sc.InvoiceDisplayNumber = req.ProviderRef // as for the notice
		sc.MandateSeqNo = p.mandateSeq(ctx, id, req.ProviderRef)
	default:
		sc.InvoiceDisplayNumber = req.ProviderRef
	}
	v1, _ := json.Marshal(sc)
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
			return d.snapshot(req.ProviderRef, req.Amount, method)
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

// RevokeMandate cancels the mandate at PayU: upi_mandate_revoke for UPI,
// mandate_revoke for cards and net banking (whose cancellation is offline: the
// answer only acknowledges it and the final status comes by webhook). A mandate
// that is already inactive counts as revoked.
func (p *payu) RevokeMandate(ctx context.Context, ref connector.MandateRef) error {
	method, err := p.mandateMethod(ctx, ref.ProviderMandateID)
	if err != nil {
		return err
	}
	cmd := "mandate_revoke"
	if method == methodUPI {
		cmd = "upi_mandate_revoke"
	}
	v1, _ := json.Marshal(mandateCall{AuthPayuID: ref.ProviderMandateID, RequestID: p.requestID()})
	var out struct {
		Status  flexStr `json:"status"`
		Msg     string  `json:"msg"`
		Message string  `json:"message"`
	}
	if err := p.api.call(ctx, cmd, string(v1), nil, &out); err != nil {
		return err
	}
	msg := firstNonEmpty(out.Msg, out.Message)
	low := strings.ToLower(msg)
	if out.Status == "1" || strings.Contains(low, "already") || strings.Contains(low, "not active") {
		return nil
	}
	if cerr := checkCredentials(msg); cerr != nil {
		return cerr
	}
	if strings.Contains(low, "not found") {
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

func (d siDetail) snapshot(ref string, fallbackAmount int64, method string) (connector.PaymentSnapshot, error) {
	amt := fallbackAmount
	if v, err := money.ParseDecimal(string(d.Amount), "INR"); err == nil && v > 0 {
		amt = v
	}
	snap := connector.PaymentSnapshot{ProviderRef: ref, ProviderPaymentID: string(d.PayUID), Status: mapStatus(string(d.Status)), RawStatus: string(d.Status),
		Amount: amt, Currency: "INR", Method: orDefault(method, methodCard)}
	if d.Status == "" {
		snap.Status = connector.PaymentFailed // PayU: a blank status is a failure
	}
	if snap.Status == connector.PaymentFailed || snap.Status == connector.PaymentCanceled {
		snap.ErrorCode, snap.ErrorMessage = "si_rejected", string(d.Field9)
	}
	return snap, nil
}
