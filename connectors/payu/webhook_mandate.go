package payu

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// Mandate webhooks. PayU sends them for what happens to a mandate after its
// registration (docs.payu.in, "Set up WebHook to Receive Cancellation or
// Modification Update from the Issuer Bank"):
//
//   - cards: a JSON body with notificationType MANDATE_MODIFICATION,
//     MANDATE_CANCELLATION, MANDATE_DELETION or MANDATE_CANCELLATION_TOKEN_DELETION,
//     signed with sha512(status|authpayuId|notificationType|billingAmount|
//     paymentStartDate|paymentEndDate|message|eventDate|key|udf1..udf5|salt);
//   - UPI: a form post with action MANDATE_PAUSE, MANDATE_UNPAUSE or MANDATE_REVOKE,
//     signed with status|action|authpayuid|dateTime|amount|endDate|salt (the
//     algorithm is not stated; SHA-512 like the others);
//   - net-banking e-NACH: a JSON cancellation notice (MANDATE_CANCELLATION,
//     status CANCEL_SUCCESS) with no hash at all.
//
// PayU has the webhook switched on by its integration team (UPI form, cards JSON,
// e-NACH through the merchant parameter notifyMandateRevoke). The debit results of
// si_transaction come as ordinary payment notices keyed by the txnid Paymux sent.
//
// TODO(spec): the two hash formulas are from the documentation; no real mandate
// notice has reached Paymux yet (the test merchant cannot register a mandate).

// mandateNotice is one parsed mandate webhook.
type mandateNotice struct {
	method                               string // card, upi or emandate
	id, status, kind, message, eventDate string
	billingAmount, startDate, endDate    string
	key, hash                            string
	udf                                  [5]string
	hashed                               string // the string the hash covers, salt excluded
}

type mandateJSON struct {
	Status           flexStr         `json:"status"`
	AuthPayuID       flexStr         `json:"authpayuid"`
	NotificationType flexStr         `json:"notificationType"`
	SIDetails        json.RawMessage `json:"si_details"`
	EventDate        flexStr         `json:"eventDate"`
	Message          flexStr         `json:"message"`
	Key              flexStr         `json:"key"`
	UDF1             flexStr         `json:"udf1"`
	UDF2             flexStr         `json:"udf2"`
	UDF3             flexStr         `json:"udf3"`
	UDF4             flexStr         `json:"udf4"`
	UDF5             flexStr         `json:"udf5"`
	Hash             flexStr         `json:"hash"`
}

// parseMandateNotice recognises a mandate webhook body. ok is false for anything
// else (payment and refund notices).
func parseMandateNotice(body []byte) (mandateNotice, bool) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var j mandateJSON
		if err := json.Unmarshal([]byte(trimmed), &j); err != nil || j.AuthPayuID == "" || !strings.HasPrefix(strings.ToUpper(string(j.NotificationType)), "MANDATE_") {
			return mandateNotice{}, false
		}
		n := mandateNotice{method: methodCard, id: string(j.AuthPayuID), status: string(j.Status), kind: string(j.NotificationType), message: string(j.Message),
			eventDate: string(j.EventDate), key: string(j.Key), hash: strings.ToLower(string(j.Hash)),
			udf: [5]string{string(j.UDF1), string(j.UDF2), string(j.UDF3), string(j.UDF4), string(j.UDF5)}}
		if len(j.SIDetails) > 0 && string(j.SIDetails) != "null" {
			raw := j.SIDetails
			var asString string
			if json.Unmarshal(raw, &asString) == nil { // a JSON object sent as a string
				raw = json.RawMessage(asString)
			}
			var si struct {
				BillingAmount    flexStr `json:"billingAmount"`
				PaymentStartDate flexStr `json:"paymentStartDate"`
				PaymentEndDate   flexStr `json:"paymentEndDate"`
			}
			if json.Unmarshal(raw, &si) == nil {
				n.billingAmount, n.startDate, n.endDate = string(si.BillingAmount), string(si.PaymentStartDate), string(si.PaymentEndDate)
			}
		}
		if n.hash == "" && n.key == "" {
			n.method = methodEMandate // the e-NACH notice carries neither key nor hash
		}
		n.hashed = strings.Join([]string{n.status, n.id, n.kind, n.billingAmount, n.startDate, n.endDate, n.message, n.eventDate, n.key,
			n.udf[0], n.udf[1], n.udf[2], n.udf[3], n.udf[4]}, "|")
		return n, true
	}
	f, err := url.ParseQuery(trimmed)
	if err != nil || f.Get("authpayuid") == "" || !strings.HasPrefix(strings.ToUpper(f.Get("action")), "MANDATE_") {
		return mandateNotice{}, false
	}
	n := mandateNotice{method: methodUPI, id: f.Get("authpayuid"), status: f.Get("status"), kind: f.Get("action"), billingAmount: f.Get("amount"),
		startDate: f.Get("dateTime"), endDate: f.Get("endDate"), eventDate: f.Get("dateTime"), key: f.Get("key"), hash: strings.ToLower(f.Get("hash"))}
	n.hashed = strings.Join([]string{n.status, n.kind, n.id, f.Get("dateTime"), f.Get("amount"), f.Get("endDate")}, "|")
	return n, true
}

// signed reports whether the notice's hash is right for this account.
func (p *payu) mandateSigned(n mandateNotice) bool {
	if n.hash == "" || n.key != p.cfg.Key {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(n.hash), []byte(sha512Hex(n.hashed, p.cfg.Salt))) == 1
}

// verifyMandateNotice is VerifyWebhook for a mandate notice: a wrong or missing
// hash on a notice that PayU signs is ErrInvalidSignature; the e-NACH notice that
// PayU never signs is ErrUnsigned (the caller treats it as a hint only).
func (p *payu) verifyMandateNotice(n mandateNotice) error {
	switch {
	case n.method == methodEMandate:
		return connector.ErrUnsigned
	case p.mandateSigned(n):
		return nil
	case n.method == methodCard && n.hash == "" && n.key == "":
		return connector.ErrUnsigned
	}
	return connector.ErrInvalidSignature
}

// mandateNotification turns a mandate notice into a Notification. A signed
// notice is used as it is; an unsigned one (e-NACH) is only a hint, so the mandate
// is read back from PayU and that is what is reported.
func (p *payu) mandateNotification(ctx context.Context, n mandateNotice) ([]connector.Notification, error) {
	status := mandateStatus(n.status)
	switch kind := strings.ToUpper(n.kind); {
	case strings.Contains(kind, "CANCELLATION"), strings.Contains(kind, "DELETION"), kind == "MANDATE_REVOKE":
		status = "revoked"
	case kind == "MANDATE_UNPAUSE":
		status = "active"
	}
	snap := connector.MandateSnapshot{ProviderMandateID: n.id, Status: status, RawStatus: n.status, Currency: "INR", Method: n.method}
	if amt, err := money.ParseDecimal(n.billingAmount, "INR"); err == nil {
		snap.MaxAmount = amt
	}
	if t, err := time.ParseInLocation("2006-01-02", n.endDate, ist); err == nil && status != "revoked" {
		snap.ValidUntil = t.UnixMilli()
	}
	if !p.mandateSigned(n) {
		truth, err := p.statusByID(ctx, n.id)
		if err != nil {
			return nil, fmt.Errorf("payu: confirming a mandate notice: %w", err)
		}
		snap = truth
	}
	return []connector.Notification{{
		Kind:       connector.NotifyMandate,
		EventID:    n.eventID(),
		EventType:  "mandate." + strings.ToLower(n.kind),
		OccurredAt: parseAddedOn(n.eventDate),
		Mandate:    &snap,
	}}, nil
}

// eventID is the notification id of a mandate notice, derived from its body.
func (n mandateNotice) eventID() string {
	return "payu:mandate:" + n.id + ":" + n.kind + ":" + n.status + ":" + n.eventDate
}

// WebhookEventID names a mandate notice from the request alone, with no call to
// PayU (connector.WebhookEventIDer): an unsigned e-NACH notice is confirmed with
// the status command only when ParseWebhook runs in the background job, never
// while the inbound request is being answered. Anything that is not a mandate
// notice is left to ParseWebhook.
func (p *payu) WebhookEventID(req connector.WebhookRequest) (string, bool) {
	n, ok := parseMandateNotice(req.Body)
	if !ok {
		return "", false
	}
	return n.eventID(), true
}

var _ connector.WebhookEventIDer = (*payu)(nil)
