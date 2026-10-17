package cashfree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

type paymentWire struct {
	CFPaymentID   json.Number     `json:"cf_payment_id"`
	OrderID       string          `json:"order_id"`
	PaymentStatus string          `json:"payment_status"`
	PaymentAmount amount          `json:"payment_amount"`
	Currency      string          `json:"payment_currency"`
	PaymentMsg    string          `json:"payment_message"`
	PaymentTime   string          `json:"payment_time"`
	PaymentGroup  string          `json:"payment_group"`
	PaymentMethod json.RawMessage `json:"payment_method"`
	ErrorDetails  *struct {
		Code        string `json:"error_code"`
		Description string `json:"error_description"`
	} `json:"error_details"`
}

// paymentID is the id Paymux stores for a payment: the order is part of it, so
// a refund (which Cashfree makes against the order) can always find it.
func paymentID(orderID string, cf json.Number) string {
	if cf == "" {
		return ""
	}
	return orderID + ":" + string(cf)
}

// splitPaymentID is the inverse of paymentID.
func splitPaymentID(id string) (orderID, cfPaymentID string, ok bool) {
	orderID, cfPaymentID, ok = strings.Cut(id, ":")
	return orderID, cfPaymentID, ok && idRE.MatchString(orderID) && idRE.MatchString(cfPaymentID)
}

// mapPaymentStatus maps Cashfree's payment_status. Unknown values stay in flight.
func mapPaymentStatus(raw string) connector.PaymentStatus {
	switch raw {
	case "SUCCESS":
		return connector.PaymentSucceeded
	case "FAILED":
		return connector.PaymentFailed
	case "USER_DROPPED", "CANCELLED", "VOID":
		return connector.PaymentCanceled
	case "NOT_ATTEMPTED":
		return connector.PaymentRequiresAction
	}
	return connector.PaymentProcessing // "PENDING" and anything new
}

// mapOrderStatus is the status of an order that has no payments yet.
func mapOrderStatus(raw string) connector.PaymentStatus {
	switch raw {
	case "ACTIVE":
		return connector.PaymentRequiresAction
	case "EXPIRED":
		return connector.PaymentExpired
	case "TERMINATED", "TERMINATION_REQUESTED":
		return connector.PaymentCanceled
	case "PAID":
		return connector.PaymentSucceeded
	}
	return connector.PaymentProcessing
}

func methodOf(p paymentWire) string {
	if p.PaymentGroup != "" {
		return p.PaymentGroup
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(p.PaymentMethod, &m) == nil {
		for k := range m {
			return k
		}
	}
	return ""
}

func parseTime(s string) int64 {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func (p paymentWire) snapshot(orderID string) connector.PaymentSnapshot {
	cur := firstNonEmpty(p.Currency, "INR")
	amt, _ := p.PaymentAmount.minor(cur)
	snap := connector.PaymentSnapshot{ProviderRef: orderID, ProviderOrderID: orderID, ProviderPaymentID: paymentID(orderID, p.CFPaymentID),
		Status: mapPaymentStatus(p.PaymentStatus), RawStatus: p.PaymentStatus, Amount: amt, Currency: cur, Method: methodOf(p), CreatedAt: parseTime(p.PaymentTime)}
	if snap.Status == connector.PaymentFailed || snap.Status == connector.PaymentCanceled {
		snap.ErrorMessage = p.PaymentMsg
		if p.ErrorDetails != nil {
			snap.ErrorCode, snap.ErrorMessage = p.ErrorDetails.Code, firstNonEmpty(p.ErrorDetails.Description, p.PaymentMsg)
		}
	}
	return snap
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func rank(p paymentWire) int {
	switch mapPaymentStatus(p.PaymentStatus) {
	case connector.PaymentSucceeded:
		return 4
	case connector.PaymentProcessing:
		return 3
	case connector.PaymentRequiresAction:
		return 2
	}
	return 1
}

// FetchPayment returns the provider's view of an order's payment. The order is
// the provider_ref (or the order id the connector returned, which is the same
// value); when both are given they must agree. Every payment listed must name
// that order_id, and only those are considered. The best payment wins: a
// success, then a pending one, then the latest failure. An order without
// payments reports its own state.
func (c *cashfree) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	if ref.ProviderPaymentID != "" {
		orderID, cfID, ok := splitPaymentID(ref.ProviderPaymentID)
		if !ok {
			return connector.PaymentSnapshot{}, connector.ErrNotFound
		}
		if (ref.ProviderRef != "" && ref.ProviderRef != orderID) || (ref.ProviderOrderID != "" && ref.ProviderOrderID != orderID) {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s belongs to another order", connector.ErrNotFound, ref.ProviderPaymentID)
		}
		var p paymentWire
		if err := c.api.do(ctx, "GET", "/orders/"+url.PathEscape(orderID)+"/payments/"+url.PathEscape(cfID), nil, &p, ""); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if p.OrderID != "" && p.OrderID != orderID {
			return connector.PaymentSnapshot{}, fmt.Errorf("%w: payment %s belongs to order %s", connector.ErrNotFound, cfID, p.OrderID)
		}
		return p.snapshot(orderID), nil
	}
	orderID := ref.ProviderRef
	if orderID == "" {
		orderID = ref.ProviderOrderID
	} else if ref.ProviderOrderID != "" && ref.ProviderOrderID != orderID {
		return connector.PaymentSnapshot{}, fmt.Errorf("%w: order %s is not the order of reference %s", connector.ErrNotFound, ref.ProviderOrderID, orderID)
	}
	if orderID == "" || len(orderID) > maxOrderID || !idRE.MatchString(orderID) {
		return connector.PaymentSnapshot{}, errors.New("cashfree: PaymentRef needs the order id (the provider_ref)")
	}
	var o orderWire
	if err := c.api.do(ctx, "GET", "/orders/"+url.PathEscape(orderID), nil, &o, ""); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	if o.OrderID != orderID {
		return connector.PaymentSnapshot{}, fmt.Errorf("%w: order %q is not %q", connector.ErrNotFound, o.OrderID, orderID)
	}
	var all []paymentWire
	if err := c.api.do(ctx, "GET", "/orders/"+url.PathEscape(orderID)+"/payments", nil, &all, ""); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	var best *paymentWire
	for i := range all {
		p := all[i]
		if p.OrderID != "" && p.OrderID != orderID {
			continue // the payments filter is not trusted: only this order's payments count
		}
		if best == nil || rank(p) > rank(*best) || (rank(p) == rank(*best) && parseTime(p.PaymentTime) > parseTime(best.PaymentTime)) {
			best = &p
		}
	}
	if best == nil {
		cur := firstNonEmpty(o.OrderCurrency, "INR")
		amt, _ := o.OrderAmount.minor(cur)
		return connector.PaymentSnapshot{ProviderRef: orderID, ProviderOrderID: orderID, Status: mapOrderStatus(o.OrderStatus), RawStatus: o.OrderStatus,
			Amount: amt, Currency: cur}, nil
	}
	return best.snapshot(orderID), nil
}

// ---- refunds ----

type refundWire struct {
	CFRefundID  json.Number `json:"cf_refund_id"`
	CFPaymentID json.Number `json:"cf_payment_id"`
	RefundID    string      `json:"refund_id"`
	OrderID     string      `json:"order_id"`
	Amount      amount      `json:"refund_amount"`
	Currency    string      `json:"refund_currency"`
	Status      string      `json:"refund_status"`
	Description string      `json:"status_description"`
	CreatedAt   string      `json:"created_at"`
	ProcessedAt string      `json:"processed_at"`
}

func mapRefundStatus(raw string) connector.RefundStatus {
	switch raw {
	case "SUCCESS":
		return connector.RefundSucceeded
	case "CANCELLED", "FAILED":
		return connector.RefundFailed
	}
	return connector.RefundPending // "PENDING", "ONHOLD" and anything new
}

func (r refundWire) snapshot() connector.RefundSnapshot {
	cur := firstNonEmpty(r.Currency, "INR")
	amt, _ := r.Amount.minor(cur)
	return connector.RefundSnapshot{ProviderRef: r.RefundID, ProviderRefundID: r.RefundID, ProviderPaymentID: paymentID(r.OrderID, r.CFPaymentID),
		Status: mapRefundStatus(r.Status), RawStatus: r.Status, Amount: amt, Currency: cur, CreatedAt: parseTime(r.CreatedAt)}
}

// Refund refunds part or all of an order's payment. The Paymux refund
// provider_ref is the refund_id (at most 40 characters) and the
// x-idempotency-key, so a retried job returns the same refund.
func (c *cashfree) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	orderID, _, ok := splitPaymentID(req.ProviderPaymentID)
	if !ok || req.Amount <= 0 {
		return connector.RefundSnapshot{}, errors.New("cashfree: refund needs a payment id (order_id:cf_payment_id) and a positive amount")
	}
	if req.ProviderRef == "" || len(req.ProviderRef) > maxRefundID || !idRE.MatchString(req.ProviderRef) {
		return connector.RefundSnapshot{}, fmt.Errorf("cashfree: provider_ref must be 1..%d letters, digits, _ or - (it is the refund_id)", maxRefundID)
	}
	amt, err := decimal(req.Amount, firstNonEmpty(req.Currency, "INR"))
	if err != nil {
		return connector.RefundSnapshot{}, fmt.Errorf("cashfree: %w", err)
	}
	in := map[string]any{"refund_amount": amt, "refund_id": req.ProviderRef, "refund_speed": "STANDARD"}
	if note := strings.TrimSpace(req.Reason); note != "" {
		if len(note) > 100 {
			note = note[:100]
		}
		in["refund_note"] = note
	}
	var w refundWire
	if err := c.api.do(ctx, "POST", "/orders/"+url.PathEscape(orderID)+"/refunds", in, &w, req.ProviderRef); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 400 && !errors.Is(err, connector.ErrNotFound) {
			return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: ae.Message}
		}
		return connector.RefundSnapshot{}, err
	}
	if w.RefundID != req.ProviderRef {
		return connector.RefundSnapshot{}, fmt.Errorf("cashfree: refund response is for refund %q, not %q", w.RefundID, req.ProviderRef)
	}
	if w.OrderID == "" {
		w.OrderID = orderID
	}
	snap := w.snapshot()
	if snap.ProviderPaymentID == "" {
		snap.ProviderPaymentID = req.ProviderPaymentID
	}
	return snap, nil
}

// FetchRefund reads a refund by its refund_id (the Paymux provider_ref) under
// the order of the payment; the refund returned must carry that refund_id.
func (c *cashfree) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	refundID := firstNonEmpty(ref.ProviderRef, ref.ProviderRefundID)
	orderID, _, ok := splitPaymentID(ref.ProviderPaymentID)
	if refundID == "" || !ok || !idRE.MatchString(refundID) {
		return connector.RefundSnapshot{}, errors.New("cashfree: RefundRef needs the refund id and the payment id (order_id:cf_payment_id)")
	}
	var w refundWire
	if err := c.api.do(ctx, "GET", "/orders/"+url.PathEscape(orderID)+"/refunds/"+url.PathEscape(refundID), nil, &w, ""); err != nil {
		return connector.RefundSnapshot{}, err
	}
	if w.RefundID != refundID || (w.OrderID != "" && w.OrderID != orderID) {
		return connector.RefundSnapshot{}, fmt.Errorf("%w: refund %q of order %q is not %q of %q", connector.ErrNotFound, w.RefundID, w.OrderID, refundID, orderID)
	}
	if w.OrderID == "" {
		w.OrderID = orderID
	}
	return w.snapshot(), nil
}

var _ connector.Refunder = (*cashfree)(nil)
