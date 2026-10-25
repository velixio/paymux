package core

import (
	"context"
	"fmt"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
)

// FlagLinkAlreadyUsed marks a payment that succeeded on a single-use payment
// link some other payment had already spent: a second sale of one item.
const FlagLinkAlreadyUsed = "link_already_used"

type alertSinkKey struct{}

// queueAlert adds an ops alert to the ones raised after the transaction the
// context belongs to commits (the hooks run inside it and cannot send mail).
func queueAlert(ctx context.Context, a notify.Alert) {
	if sink, ok := ctx.Value(alertSinkKey{}).(*[]notify.Alert); ok {
		*sink = append(*sink, a)
	}
}

// MarkExtraSale deals with a payment that succeeded although what it paid for
// was already sold (a single-use link spent by another payment). Inside the
// transaction in which it became succeeded: it is flagged, and following
// duplicate_payment_action it is refunded in full automatically or left for an
// operator, with a payment.needs_review event and an alert.
func (s *Service) MarkExtraSale(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt, reason string) error {
	cur, err := store.GetPayment(ctx, tx, p.ID) // the hook's copy may be a version behind
	if err != nil {
		return mapStoreErr(err, "payment")
	}
	p = cur
	addFlag(&p, FlagLinkAlreadyUsed)
	if err := store.UpdatePayment(ctx, tx, &p, s.nowUTC()); err != nil {
		return err
	}
	v, err := s.policy(ctx, tx, p.AppID, "duplicate_payment_action")
	if err != nil {
		return err
	}
	var action string
	if err := v.Decode(&action); err != nil {
		return err
	}
	refs := map[string]string{"payment": p.ID, "attempt": a.ID, "app": p.AppID}
	if action == "auto_refund" && a.AmountReceived > 0 {
		if _, err := s.insertRefund(ctx, tx, p, a, a.AmountReceived, reason, "system", "", audit.System); err != nil {
			return err
		}
		queueAlert(ctx, notify.Alert{Severity: notify.SeverityInfo, Kind: "payment.link_reused_refunded",
			Message: fmt.Sprintf("Payment %s succeeded on a single-use payment link that was already used; %d %s is being refunded automatically.", p.ID, a.AmountReceived, a.CurrencyReceived),
			Refs:    refs})
		return nil
	}
	queueAlert(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: "payment.link_reused",
		Message: fmt.Sprintf("Payment %s succeeded on a single-use payment link that was already used. Check it and refund it from the dashboard if the item was sold once.", p.ID),
		Refs:    refs})
	return s.emitPayment(ctx, tx, "payment.needs_review", p, FlagLinkAlreadyUsed, a.ID)
}

// CancelOpenOrderPayments cancels the payments of this order reference that can
// still be paid, except one: when a single-use link is spent, the other payments
// started on it must not be paid as well. Each cancellation is an ordinary
// payment.canceled. A payment already on its way at the provider that succeeds
// later is then handled as a success after cancel: held for review, never refunded
// automatically.
func (s *Service) CancelOpenOrderPayments(ctx context.Context, tx store.Tx, appID, mode, orderRef, exceptID string) (int, error) {
	ids, err := store.OpenPaymentsOfOrder(ctx, tx, appID, mode, orderRef)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if id == exceptID {
			continue
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, id)
		if err != nil {
			return n, mapStoreErr(err, "payment")
		}
		out := state.PaymentNext(state.PaymentStatus(p.Status), state.EvCancel)
		if out.Action != state.Apply {
			continue
		}
		p.Status = string(out.Next)
		if err := store.UpdatePayment(ctx, tx, &p, s.nowUTC()); err != nil {
			return n, err
		}
		if err := s.emitPayment(ctx, tx, "payment.canceled", p, "payment_link_spent", ""); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ReissueClientSecret gives an open payment a new client secret and returns it:
// the secret is kept only as a hash, so a visitor who returns to a payment they
// started gets a fresh one (the old one stops working). A payment that is over
// is ErrInvalidState.
func (s *Service) ReissueClientSecret(ctx context.Context, paymentID string) (string, error) {
	secret := newClientSecret()
	err := s.db.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPaymentForUpdate(ctx, tx, paymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		switch state.PaymentStatus(p.Status) {
		case state.PaymentCreated, state.PaymentRequiresAction, state.PaymentProcessing:
		default:
			return newErr(ErrInvalidState, "a payment that is %s has no use for a new secret", p.Status)
		}
		return store.SetPaymentSecretHash(ctx, tx, p.ID, hashSecret(secret), s.nowUTC())
	})
	return secret, err
}
