package payu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// errDuplicateToken is the error_code of a refund token reused with the same amount.
const errDuplicateToken = "227"

type client struct {
	http *http.Client
	key  string
	salt string
	test bool
}

func (c *client) paymentURL() string {
	if c.test {
		return "https://test.payu.in/_payment"
	}
	return "https://secure.payu.in/_payment"
}

func (c *client) serviceURL() string {
	if c.test {
		return "https://test.payu.in/merchant/postservice.php?form=2"
	}
	return "https://info.payu.in/merchant/postservice.php?form=2"
}

// flexStr decodes a JSON string, number or null into a string.
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flexStr(v)
		return nil
	}
	*f = flexStr(s)
	return nil
}

// call posts a postservice command: hash = sha512(key|command|var1|salt).
func (c *client) call(ctx context.Context, command, var1 string, extra map[string]string, out any) error {
	form := url.Values{
		"key": {c.key}, "command": {command}, "var1": {var1},
		"hash": {sha512Hex(c.key, command, var1, c.salt)},
	}
	for k, v := range extra {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.serviceURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%w: %v", connector.ErrTemporary, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", connector.ErrTemporary, err)
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return connector.ErrInvalidCredentials
	case resp.StatusCode == 429:
		after := 5 * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return connector.ErrRateLimited{RetryAfter: after}
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: HTTP %d", connector.ErrTemporary, resp.StatusCode)
	case resp.StatusCode != 200:
		return fmt.Errorf("payu: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("payu: decoding %s response: %w", command, err)
	}
	return nil
}

// checkCredentials maps PayU's "invalid hash/key" messages to ErrInvalidCredentials.
func checkCredentials(msg string) error {
	l := strings.ToLower(msg)
	if strings.Contains(l, "invalid hash") || strings.Contains(l, "invalid key") || strings.Contains(l, "authentication") {
		return fmt.Errorf("%w: %s", connector.ErrInvalidCredentials, msg)
	}
	return nil
}

type txnDetail struct {
	MihPayID       flexStr `json:"mihpayid"`
	ID             flexStr `json:"id"`         // get_Transaction_Details names the PayU id "id"
	RequestID      flexStr `json:"request_id"` // set on the row PayU adds for a refund
	ServiceFee     flexStr `json:"mer_service_fee"`
	ServiceTax     flexStr `json:"mer_service_tax"`
	TxnID          flexStr `json:"txnid"`
	Status         flexStr `json:"status"`
	UnmappedStatus flexStr `json:"unmappedstatus"`
	Amt            flexStr `json:"amt"`
	Amount         flexStr `json:"amount"`
	TxnAmount      flexStr `json:"transaction_amount"`
	Mode           flexStr `json:"mode"`
	ErrorCode      flexStr `json:"error_code"`
	ErrorMessage   flexStr `json:"error_Message"`
	AddedOn        flexStr `json:"addedon"`
	PaymentSource  flexStr `json:"payment_source"` // "sist" when the registration created a mandate
}

func (d txnDetail) snapshot(txnid string) (connector.PaymentSnapshot, error) {
	amt := firstNonEmpty(string(d.Amt), string(d.Amount), string(d.TxnAmount))
	var minor int64
	if amt != "" {
		var err error
		if minor, err = money.ParseDecimal(amt, "INR"); err != nil {
			return connector.PaymentSnapshot{}, fmt.Errorf("payu: amount %q: %w", amt, err)
		}
	}
	snap := connector.PaymentSnapshot{
		ProviderRef: txnid, ProviderPaymentID: firstNonEmpty(string(d.MihPayID), string(d.ID)),
		Status: mapStatus(string(d.Status)), RawStatus: string(d.Status),
		Amount: minor, Currency: "INR", Method: methodOf(string(d.Mode)), CreatedAt: parseAddedOn(string(d.AddedOn)),
	}
	if fee, err := money.ParseDecimal(string(d.ServiceFee), "INR"); err == nil {
		snap.Fee = fee
	}
	if tax, err := money.ParseDecimal(string(d.ServiceTax), "INR"); err == nil {
		snap.Tax = tax
	}
	if snap.ProviderPaymentID == "Not Found" {
		snap.ProviderPaymentID = "" // PayU's placeholder for a txnid it has no payment for
	}
	// PayU sends error_code E000 and "NO ERROR" on success: only a failure has an error.
	if snap.Status == connector.PaymentFailed || snap.Status == connector.PaymentCanceled {
		snap.ErrorCode, snap.ErrorMessage = string(d.ErrorCode), string(d.ErrorMessage)
	}
	return snap, nil
}

// methodOf maps PayU's mode (CC, DC, NB, UPI, WALLET, EMI, CASH...) to the method
// names Paymux uses. An unknown mode is passed on in lower case.
func methodOf(mode string) string {
	switch strings.ToUpper(mode) {
	case "CC", "DC":
		return "card"
	case "NB":
		return "netbanking"
	case "UPI":
		return "upi"
	case "WALLET", "CASH":
		return "wallet"
	case "EMI":
		return "emi"
	}
	return strings.ToLower(mode)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// FetchPayment calls verify_payment for the txnid (provider_ref). It is the
// poller for the "pending" status, which PayU never settles by webhook alone.
func (p *payu) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	if ref.ProviderRef == "" {
		return connector.PaymentSnapshot{}, errors.New("payu: FetchPayment needs ProviderRef (txnid)")
	}
	var out struct {
		Status  flexStr              `json:"status"`
		Msg     string               `json:"msg"`
		Details map[string]txnDetail `json:"transaction_details"`
	}
	if err := p.api.call(ctx, "verify_payment", ref.ProviderRef, nil, &out); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	d, ok := out.Details[ref.ProviderRef]
	if !ok {
		if err := checkCredentials(out.Msg); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		return connector.PaymentSnapshot{}, connector.ErrNotFound
	}
	return d.snapshot(ref.ProviderRef)
}

// Refund calls cancel_refund_transaction: var1 = payu id, var2 = token (the
// refund provider_ref, max 23 chars, PayU's idempotency key), var3 = amount.
func (p *payu) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	if req.ProviderRef == "" || len(req.ProviderRef) > maxRefundTokenLen {
		return connector.RefundSnapshot{}, fmt.Errorf("payu: refund provider_ref must be 1..%d chars", maxRefundTokenLen)
	}
	if req.ProviderPaymentID == "" || req.Amount <= 0 {
		return connector.RefundSnapshot{}, errors.New("payu: refund needs a payment id and a positive amount")
	}
	amt, err := money.FormatDecimal(req.Amount, "INR")
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	var out struct {
		Status    flexStr `json:"status"`
		Msg       string  `json:"msg"`
		RequestID flexStr `json:"request_id"`
		ErrorCode flexStr `json:"error_code"`
	}
	err = p.api.call(ctx, "cancel_refund_transaction", req.ProviderPaymentID,
		map[string]string{"var2": req.ProviderRef, "var3": amt}, &out)
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	if out.Status != "1" {
		if cerr := checkCredentials(out.Msg); cerr != nil {
			return connector.RefundSnapshot{}, cerr
		}
		if string(out.ErrorCode) == errDuplicateToken {
			// error_code 227 ("same amount and same token not allowed") is PayU's own
			// deduplication: a retried job. Adopt the refund it already made.
			got, ferr := p.FetchRefund(ctx, connector.RefundRef{ProviderRef: req.ProviderRef, ProviderPaymentID: req.ProviderPaymentID})
			if ferr != nil {
				return connector.RefundSnapshot{}, fmt.Errorf("payu: refund token %s exists but cannot be read: %w", req.ProviderRef, ferr)
			}
			if got.Amount != req.Amount {
				return connector.RefundSnapshot{}, fmt.Errorf("payu: refund token %s already used for %d, not %d", req.ProviderRef, got.Amount, req.Amount)
			}
			return got, nil
		}
		return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: out.Msg}
	}
	return connector.RefundSnapshot{
		ProviderRef: req.ProviderRef, ProviderRefundID: string(out.RequestID), ProviderPaymentID: req.ProviderPaymentID,
		Status: connector.RefundPending, RawStatus: out.Msg, Amount: req.Amount, Currency: "INR",
	}, nil
}

func mapRefundStatus(raw string) connector.RefundStatus {
	switch strings.ToLower(raw) {
	case "success", "refunded", "processed":
		return connector.RefundSucceeded
	case "failure", "failed", "rejected":
		return connector.RefundFailed
	}
	return connector.RefundPending
}

// actionEntry is one action (capture, refund) of a payment in check_action_status.
type actionEntry struct {
	MihPayID  flexStr `json:"mihpayid"`
	RequestID flexStr `json:"request_id"`
	Action    flexStr `json:"action"`
	Token     flexStr `json:"token"`
	Status    flexStr `json:"status"`
	Amt       flexStr `json:"amt"`
}

// FetchRefund reads a refund from check_action_status, which has two forms
// (both verified against the test environment):
//
//   - by request id (var1 = the request_id Refund returned): the answer is nested
//     as transaction_details.<request_id>.<request_id>;
//   - by payment (var1 = the PayU id, var2 = "payuid"): every action of the
//     payment, the capture included, keyed by request id. A refund is picked by
//     its token, the Paymux refund reference.
//
// An entry is accepted only when its request id (or token) is the one asked for,
// and only when it is a refund; nothing is guessed at. A lookup by token alone
// is impossible: PayU does not accept a token as an id.
func (p *payu) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	var (
		var1   string
		extra  map[string]string
		accept func(actionEntry) bool
	)
	switch {
	case ref.ProviderRefundID != "":
		id := ref.ProviderRefundID
		var1, accept = id, func(e actionEntry) bool { return string(e.RequestID) == id }
	case ref.ProviderRef != "" && ref.ProviderPaymentID != "":
		token := ref.ProviderRef
		var1, extra, accept = ref.ProviderPaymentID, map[string]string{"var2": "payuid"}, func(e actionEntry) bool { return string(e.Token) == token }
	default:
		return connector.RefundSnapshot{}, errors.New("payu: RefundRef needs a request id, or a token with the payment id")
	}
	var out struct {
		Status  flexStr                    `json:"status"`
		Msg     string                     `json:"msg"`
		Details map[string]json.RawMessage `json:"transaction_details"`
	}
	if err := p.api.call(ctx, "check_action_status", var1, extra, &out); err != nil {
		return connector.RefundSnapshot{}, err
	}
	var entries []actionEntry
	var walk func(raw json.RawMessage)
	walk = func(raw json.RawMessage) {
		var e actionEntry
		if json.Unmarshal(raw, &e) == nil && e.Status != "" {
			entries = append(entries, e)
			return
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			for _, v := range nested {
				walk(v)
			}
		}
	}
	for _, raw := range out.Details {
		walk(raw)
	}
	var found *actionEntry
	for i := range entries {
		e := &entries[i]
		if (e.Action == "" || strings.EqualFold(string(e.Action), "refund")) && accept(*e) {
			found = e
			break
		}
	}
	if found == nil {
		if len(entries) == 0 {
			if err := checkCredentials(out.Msg); err != nil {
				return connector.RefundSnapshot{}, err
			}
		}
		return connector.RefundSnapshot{}, connector.ErrNotFound
	}
	var minor int64
	if found.Amt != "" {
		var err error
		if minor, err = money.ParseDecimal(string(found.Amt), "INR"); err != nil {
			return connector.RefundSnapshot{}, err
		}
	}
	return connector.RefundSnapshot{
		ProviderRef: firstNonEmpty(string(found.Token), ref.ProviderRef), ProviderRefundID: string(found.RequestID),
		ProviderPaymentID: firstNonEmpty(string(found.MihPayID), ref.ProviderPaymentID),
		Status:            mapRefundStatus(string(found.Status)), RawStatus: string(found.Status), Amount: minor, Currency: "INR",
	}, nil
}

// ListPayments uses get_Transaction_Details over IST calendar dates and
// filters to the exact [From,To) range. It returns one page (no cursor);
// callers should pass short ranges.
func (p *payu) ListPayments(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	var pg connector.Page[connector.PaymentSnapshot]
	if cur != "" {
		return pg, fmt.Errorf("payu: bad cursor %q", cur)
	}
	from := time.UnixMilli(tr.From).In(ist).Format("2006-01-02")
	to := time.UnixMilli(tr.To - 1).In(ist).Format("2006-01-02")
	var out struct {
		Status  flexStr     `json:"status"`
		Msg     string      `json:"msg"`
		Details []txnDetail `json:"Transaction_details"`
	}
	// For this command var1 is the from-date, which is what the hash covers.
	if err := p.api.call(ctx, "get_Transaction_Details", from, map[string]string{"var2": to}, &out); err != nil {
		return pg, err
	}
	if out.Status != "1" {
		if err := checkCredentials(out.Msg); err != nil {
			return pg, err
		}
	}
	for _, d := range out.Details {
		if d.RequestID != "" {
			continue // a refund's row repeats the payment (same id, txnid, amount and status): not a payment
		}
		s, err := d.snapshot(string(d.TxnID))
		if err != nil {
			return pg, err
		}
		if s.CreatedAt != 0 && (s.CreatedAt < tr.From || s.CreatedAt >= tr.To) {
			continue
		}
		pg.Items = append(pg.Items, s)
	}
	return pg, nil
}

// ListRefunds is not supported: PayU exposes no refund listing API that Paymux
// uses. (A refund made in the dashboard is found by listing a payment's actions
// with check_action_status, var2=payuid; see FetchRefund.)
func (p *payu) ListRefunds(context.Context, connector.TimeRange, connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	return connector.Page[connector.RefundSnapshot]{}, connector.ErrUnsupported
}
