package payu

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// ist is Asia/Kolkata without needing tzdata; PayU timestamps are IST.
var ist = time.FixedZone("IST", 5*3600+1800)

func parseAddedOn(s string) int64 {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, ist)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func mapStatus(raw string) connector.PaymentStatus {
	switch raw {
	case "success", "captured": // "captured" is the unmapped status (listings)
		return connector.PaymentSucceeded
	case "auth":
		return connector.PaymentAuthorized
	case "failure", "failed", "bounced":
		return connector.PaymentFailed
	case "userCancelled", "usercancelled", "dropped":
		return connector.PaymentCanceled
	case "Not Found", "not found":
		return connector.PaymentRequiresAction
	default: // "pending", "in progress" and anything new
		return connector.PaymentProcessing
	}
}

// VerifyWebhook checks the reverse hash of a form-urlencoded payment webhook.
// A missing, wrong or unparsable hash on a payment webhook is
// ErrInvalidSignature. Only PayU refund (and other non-payment) notices that
// carry no hash field at all are ErrUnsigned: the provider does not sign
// them, so the caller polls FetchRefund instead of trusting them.
func (p *payu) VerifyWebhook(_ context.Context, req connector.WebhookRequest) error {
	f, err := url.ParseQuery(string(req.Body))
	if err != nil {
		return connector.ErrInvalidSignature
	}
	if f.Get("hash") == "" && f.Get("txnid") == "" {
		return connector.ErrUnsigned // an unsigned refund notice
	}
	if !p.verifyReverse(f) {
		return connector.ErrInvalidSignature
	}
	return nil
}

// ParseWebhook turns a verified payment webhook into a Notification. PayU has
// no event id, so one is synthesized from the payu id and the statuses; a
// retried delivery therefore dedupes while a later status change does not.
func (p *payu) ParseWebhook(_ context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	f, err := url.ParseQuery(string(req.Body))
	if err != nil {
		return nil, fmt.Errorf("payu: bad webhook body: %w", err)
	}
	if f.Get("txnid") == "" || f.Get("status") == "" {
		return nil, nil // not a payment event (e.g. refund or dispute notice)
	}
	amount, err := money.ParseDecimal(f.Get("amount"), "INR")
	if err != nil {
		return nil, fmt.Errorf("payu: webhook amount: %w", err)
	}
	snap := connector.PaymentSnapshot{
		ProviderRef: f.Get("txnid"), ProviderPaymentID: f.Get("mihpayid"),
		Status: mapStatus(f.Get("status")), RawStatus: f.Get("status"),
		Amount: amount, Currency: "INR", Method: methodOf(f.Get("mode")), CreatedAt: parseAddedOn(f.Get("addedon")),
	}
	if snap.Status == connector.PaymentFailed || snap.Status == connector.PaymentCanceled {
		snap.ErrorCode, snap.ErrorMessage = f.Get("error"), f.Get("error_Message")
	}
	return []connector.Notification{{
		Kind:       connector.NotifyPayment,
		EventID:    "payu:" + f.Get("mihpayid") + ":" + f.Get("status") + ":" + f.Get("unmappedstatus"),
		EventType:  "payment." + f.Get("status"),
		OccurredAt: snap.CreatedAt,
		Payment:    &snap,
	}}, nil
}
