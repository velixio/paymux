package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/velixio/paymux/pkg/connector"
)

// Wire entities (only the fields Paymux reads). Timestamps are unix seconds.
type session struct {
	ID                string                       `json:"id"`
	Mode              string                       `json:"mode"`
	Status            string                       `json:"status"`         // open | complete | expired
	PaymentStatus     string                       `json:"payment_status"` // paid | unpaid | no_payment_required
	ClientReferenceID string                       `json:"client_reference_id"`
	AmountTotal       int64                        `json:"amount_total"`
	Currency          string                       `json:"currency"`
	URL               string                       `json:"url"`
	ExpiresAt         int64                        `json:"expires_at"`
	Created           int64                        `json:"created"`
	Metadata          map[string]string            `json:"metadata"`
	PaymentIntent     expandable[paymentIntent]    `json:"payment_intent"`
	Subscription      expandable[subscriptionWire] `json:"subscription"`
}

type paymentIntent struct {
	ID            string               `json:"id"`
	Status        string               `json:"status"`
	Amount        int64                `json:"amount"`
	Currency      string               `json:"currency"`
	Created       int64                `json:"created"`
	Metadata      map[string]string    `json:"metadata"`
	Invoice       expandable[struct{}] `json:"invoice"`
	LatestCharge  expandable[charge]   `json:"latest_charge"`
	LastPayError  *paymentError        `json:"last_payment_error"`
	CaptureMethod string               `json:"capture_method"`
}

type paymentError struct {
	Code        string `json:"code"`
	DeclineCode string `json:"decline_code"`
	Message     string `json:"message"`
}

type charge struct {
	ID                   string `json:"id"`
	AmountRefunded       int64  `json:"amount_refunded"`
	PaymentMethodDetails struct {
		Type string `json:"type"`
	} `json:"payment_method_details"`
}

type refundWire struct {
	ID            string               `json:"id"`
	Status        string               `json:"status"`
	Amount        int64                `json:"amount"`
	Currency      string               `json:"currency"`
	Created       int64                `json:"created"`
	Metadata      map[string]string    `json:"metadata"`
	PaymentIntent expandable[struct{}] `json:"payment_intent"`
	Charge        expandable[struct{}] `json:"charge"`
}

func upper(s string) string { return strings.ToUpper(s) }

// mapSession decides a one-time payment's status from its Checkout Session and
// PaymentIntent. Unknown values map to the safe in-flight status.
func mapSession(s session) connector.PaymentStatus {
	pi := s.PaymentIntent.Obj
	switch {
	case s.PaymentStatus == "paid" || s.PaymentStatus == "no_payment_required":
		return connector.PaymentSucceeded
	case pi != nil && pi.Status == "succeeded":
		return connector.PaymentSucceeded
	case pi != nil && pi.Status == "requires_capture":
		return connector.PaymentAuthorized
	case pi != nil && pi.Status == "canceled":
		return connector.PaymentCanceled
	case s.Status == "expired":
		return connector.PaymentExpired
	case s.Status == "complete": // completed but not yet paid: a delayed method (bank debit)
		return connector.PaymentProcessing
	case s.Status == "open", s.Status == "":
		return connector.PaymentRequiresAction // the customer has not finished (a failed card can be retried in the same session)
	}
	return connector.PaymentProcessing
}

func mapIntent(pi paymentIntent) connector.PaymentStatus {
	switch pi.Status {
	case "succeeded":
		return connector.PaymentSucceeded
	case "requires_capture":
		return connector.PaymentAuthorized
	case "canceled":
		return connector.PaymentCanceled
	case "requires_payment_method", "requires_confirmation", "requires_action":
		return connector.PaymentRequiresAction
	}
	return connector.PaymentProcessing // "processing" and anything new
}

func chargeOf(pi *paymentIntent) *charge {
	if pi == nil {
		return nil
	}
	return pi.LatestCharge.Obj
}

func (s session) snapshot() connector.PaymentSnapshot {
	snap := connector.PaymentSnapshot{ProviderRef: s.ClientReferenceID, ProviderOrderID: s.ID, ProviderPaymentID: s.PaymentIntent.ID,
		Status: mapSession(s), RawStatus: s.Status + "/" + s.PaymentStatus, Amount: fromStripe(s.AmountTotal, s.Currency),
		Currency: upper(s.Currency), CreatedAt: ms(s.Created)}
	if pi := s.PaymentIntent.Obj; pi != nil {
		if pi.LastPayError != nil && snap.Status == connector.PaymentRequiresAction {
			snap.ErrorCode, snap.ErrorMessage = firstNonEmpty(pi.LastPayError.DeclineCode, pi.LastPayError.Code), pi.LastPayError.Message
		}
		if c := chargeOf(pi); c != nil {
			snap.Method = c.PaymentMethodDetails.Type
			snap.AmountRefunded = fromStripe(c.AmountRefunded, s.Currency)
		}
	}
	return snap
}

func (pi paymentIntent) snapshot() connector.PaymentSnapshot {
	snap := connector.PaymentSnapshot{ProviderRef: pi.Metadata[metaRef], ProviderPaymentID: pi.ID, Status: mapIntent(pi), RawStatus: pi.Status,
		Amount: fromStripe(pi.Amount, pi.Currency), Currency: upper(pi.Currency), CreatedAt: ms(pi.Created)}
	if pi.LastPayError != nil {
		snap.ErrorCode, snap.ErrorMessage = firstNonEmpty(pi.LastPayError.DeclineCode, pi.LastPayError.Code), pi.LastPayError.Message
	}
	if c := pi.LatestCharge.Obj; c != nil {
		snap.Method = c.PaymentMethodDetails.Type
		snap.AmountRefunded = fromStripe(c.AmountRefunded, pi.Currency)
	}
	return snap
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *stripe) getSession(ctx context.Context, id string) (session, error) {
	var sess session
	q := url.Values{"expand[]": {"payment_intent", "payment_intent.latest_charge"}}
	err := s.api.do(ctx, "GET", "/v1/checkout/sessions/"+url.PathEscape(id), q, nil, &sess, "")
	return sess, err
}

// FetchPayment returns the provider's view of a payment, matched strictly: the
// checkout session (or PaymentIntent) is fetched by the id this connector
// created and returned, and when the caller names a provider_ref it must equal
// the reference stored on that object. A mismatch is ErrNotFound. Nothing is
// ever searched for.
func (s *stripe) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	switch {
	case ref.ProviderOrderID != "":
		if !strings.HasPrefix(ref.ProviderOrderID, "cs_") {
			return connector.PaymentSnapshot{}, connector.ErrNotFound
		}
		sess, err := s.getSession(ctx, ref.ProviderOrderID)
		if err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if ref.ProviderRef != "" && sess.ClientReferenceID != ref.ProviderRef {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: session %s belongs to another payment", connector.ErrNotFound, sess.ID)
		}
		if ref.ProviderPaymentID != "" && sess.PaymentIntent.ID != "" && sess.PaymentIntent.ID != ref.ProviderPaymentID {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: session %s has another payment intent", connector.ErrNotFound, sess.ID)
		}
		snap := sess.snapshot()
		if snap.ProviderRef == "" {
			snap.ProviderRef = ref.ProviderRef
		}
		return snap, nil
	case ref.ProviderPaymentID != "":
		if !strings.HasPrefix(ref.ProviderPaymentID, "pi_") {
			return connector.PaymentSnapshot{}, connector.ErrNotFound
		}
		var pi paymentIntent
		q := url.Values{"expand[]": {"latest_charge"}}
		if err := s.api.do(ctx, "GET", "/v1/payment_intents/"+url.PathEscape(ref.ProviderPaymentID), q, nil, &pi, ""); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if ref.ProviderRef != "" && pi.Metadata[metaRef] != ref.ProviderRef {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment intent %s belongs to another payment", connector.ErrNotFound, pi.ID)
		}
		snap := pi.snapshot()
		if snap.ProviderRef == "" {
			snap.ProviderRef = ref.ProviderRef
		}
		return snap, nil
	}
	return connector.PaymentSnapshot{}, errors.New("stripe: PaymentRef needs the checkout session id (payments are never searched for)")
}

// ---- refunds ----

func mapRefundStatus(raw string) connector.RefundStatus {
	switch raw {
	case "succeeded":
		return connector.RefundSucceeded
	case "failed", "canceled":
		return connector.RefundFailed
	}
	return connector.RefundPending // "pending", "requires_action" and anything new
}

func (r refundWire) snapshot() connector.RefundSnapshot {
	pay := r.PaymentIntent.ID
	if pay == "" {
		pay = r.Charge.ID
	}
	return connector.RefundSnapshot{ProviderRef: r.Metadata[metaRef], ProviderRefundID: r.ID, ProviderPaymentID: pay,
		Status: mapRefundStatus(r.Status), RawStatus: r.Status, Amount: fromStripe(r.Amount, r.Currency), Currency: upper(r.Currency),
		CreatedAt: ms(r.Created)}
}

// stripeReasons are the refund reasons Stripe accepts; any other reason is only
// kept in the metadata.
var stripeReasons = map[string]bool{"duplicate": true, "fraudulent": true, "requested_by_customer": true}

// Refund creates a refund. The Paymux refund provider_ref is sent as the
// Idempotency-Key and stored in the refund's metadata, so a retried job
// returns the same refund instead of creating a second one.
func (s *stripe) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	if req.ProviderPaymentID == "" || req.Amount <= 0 || req.ProviderRef == "" {
		return connector.RefundSnapshot{}, errors.New("stripe: refund needs a payment id, a reference and a positive amount")
	}
	amount, err := toStripe(req.Amount, req.Currency)
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	f := url.Values{}
	if strings.HasPrefix(req.ProviderPaymentID, "ch_") {
		f.Set("charge", req.ProviderPaymentID)
	} else {
		f.Set("payment_intent", req.ProviderPaymentID)
	}
	f.Set("amount", fmt.Sprint(amount))
	f.Set("metadata["+metaRef+"]", req.ProviderRef)
	if req.Reason != "" {
		if stripeReasons[req.Reason] {
			f.Set("reason", req.Reason)
		}
		f.Set("metadata[reason]", req.Reason)
	}
	var w refundWire
	if err := s.api.do(ctx, "POST", "/v1/refunds", nil, f, &w, req.ProviderRef); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 400 && !errors.Is(err, connector.ErrNotFound) {
			return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: ae.Message}
		}
		return connector.RefundSnapshot{}, err
	}
	snap := w.snapshot()
	snap.ProviderRef = req.ProviderRef
	if snap.Currency == "" {
		snap.Currency = upper(req.Currency)
	}
	return snap, nil
}

// FetchRefund looks a refund up by its Stripe id, or, with the payment's id and
// the Paymux reference, among the payment's refunds by the reference stored in
// their metadata.
func (s *stripe) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	if ref.ProviderRefundID != "" {
		var w refundWire
		if err := s.api.do(ctx, "GET", "/v1/refunds/"+url.PathEscape(ref.ProviderRefundID), nil, nil, &w, ""); err != nil {
			return connector.RefundSnapshot{}, err
		}
		if ref.ProviderRef != "" && w.Metadata[metaRef] != ref.ProviderRef {
			return connector.RefundSnapshot{}, fmt.Errorf("%w: refund %s belongs to another refund", connector.ErrNotFound, w.ID)
		}
		return w.snapshot(), nil
	}
	if ref.ProviderRef == "" || ref.ProviderPaymentID == "" {
		return connector.RefundSnapshot{}, errors.New("stripe: RefundRef needs a refund id, or a reference and a payment id")
	}
	q := url.Values{"limit": {"100"}}
	if strings.HasPrefix(ref.ProviderPaymentID, "ch_") {
		q.Set("charge", ref.ProviderPaymentID)
	} else {
		q.Set("payment_intent", ref.ProviderPaymentID)
	}
	var l list[refundWire]
	if err := s.api.do(ctx, "GET", "/v1/refunds", q, nil, &l, ""); err != nil {
		return connector.RefundSnapshot{}, err
	}
	for _, w := range l.Data {
		if w.Metadata[metaRef] == ref.ProviderRef {
			return w.snapshot(), nil
		}
	}
	return connector.RefundSnapshot{}, connector.ErrNotFound
}

// ---- listings ----

const listPageSize = 100

// listQuery builds the created range and the cursor. The SDK range is half-open
// milliseconds; Stripe takes unix seconds.
func listQuery(tr connector.TimeRange, cur connector.Cursor, dateField string) url.Values {
	q := url.Values{"limit": {strconv.Itoa(listPageSize)}}
	if tr.From > 0 {
		q.Set(dateField+"[gte]", strconv.FormatInt(tr.From/1000, 10))
	}
	if tr.To > 0 {
		q.Set(dateField+"[lt]", strconv.FormatInt((tr.To+999)/1000, 10))
	}
	if cur != "" {
		q.Set("starting_after", string(cur))
	}
	return q
}

// ListPayments lists PaymentIntents created in the range, one page at a time.
func (s *stripe) ListPayments(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	q := listQuery(tr, cur, "created")
	q["expand[]"] = []string{"data.latest_charge"}
	var l list[paymentIntent]
	if err := s.api.do(ctx, "GET", "/v1/payment_intents", q, nil, &l, ""); err != nil {
		return connector.Page[connector.PaymentSnapshot]{}, err
	}
	pg := connector.Page[connector.PaymentSnapshot]{Items: make([]connector.PaymentSnapshot, 0, len(l.Data))}
	for _, pi := range l.Data {
		pg.Items = append(pg.Items, pi.snapshot())
	}
	if l.HasMore && len(l.Data) > 0 {
		pg.Next = connector.Cursor(l.Data[len(l.Data)-1].ID)
	}
	return pg, nil
}

// ListRefunds lists refunds created in the range, one page at a time.
func (s *stripe) ListRefunds(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	var l list[refundWire]
	if err := s.api.do(ctx, "GET", "/v1/refunds", listQuery(tr, cur, "created"), nil, &l, ""); err != nil {
		return connector.Page[connector.RefundSnapshot]{}, err
	}
	pg := connector.Page[connector.RefundSnapshot]{Items: make([]connector.RefundSnapshot, 0, len(l.Data))}
	for _, w := range l.Data {
		pg.Items = append(pg.Items, w.snapshot())
	}
	if l.HasMore && len(l.Data) > 0 {
		pg.Next = connector.Cursor(l.Data[len(l.Data)-1].ID)
	}
	return pg, nil
}

var (
	_ connector.Refunder = (*stripe)(nil)
	_ connector.Lister   = (*stripe)(nil)
)
