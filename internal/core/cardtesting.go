package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
)

// Card-testing flags. FlagCardTesting marks a payment that stopped taking new
// attempts after card_decline_limit different cards were declined on it. The
// other two are review reasons: a success after review_after_declined_cards
// declined cards, and a success by a blocked customer.
const (
	FlagCardTesting     = "card_testing"
	FlagDeclinedCards   = "declined_cards"
	FlagCustomerBlocked = "customer_blocked"
)

// reviewReasons are the needs_review reasons an operator may approve: the
// money arrived in full, only the buyer is in doubt.
var reviewReasons = []string{FlagCardTesting, FlagDeclinedCards, FlagCustomerBlocked, FlagVPN, FlagIPMismatch, FlagAddressMismatch}

// BlockReasonDeclinedCards is the customer block reason set by the
// customer_decline_limit rule. An operator's block has the reason "operator".
const (
	BlockReasonDeclinedCards = "declined_cards"
	BlockReasonOperator      = "operator"
)

// merchantInitiated reports a payment charged with no customer present (a
// subscription renewal). Card testing needs a customer at a checkout, so the
// rules below leave these payments alone.
func merchantInitiated(p store.Payment) bool { return p.Kind == "renewal" }

func (s *Service) intPolicy(ctx context.Context, q store.Queryer, appID, key string) (int, error) {
	v, err := s.policy(ctx, q, appID, key)
	if err != nil {
		return 0, err
	}
	var n int
	if err := v.Decode(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// isDecline reports whether a snapshot is a declined try. A provider may keep
// the checkout open after a decline, so a try can fail with the attempt still
// pending.
func isDecline(snap Snapshot) bool {
	return snap.Status == state.AttemptFailed || snap.FailureCode != "" || snap.FailureMessage != ""
}

// snapCardKey is store.CardKey for the card of a snapshot.
func snapCardKey(snap Snapshot) string {
	raw := ""
	if !snap.MethodDetails.Empty() {
		if b, err := json.Marshal(snap.MethodDetails); err == nil {
			raw = string(b)
		}
	}
	return store.CardKey(raw, snap.ProviderPaymentID)
}

// declineHistory is the declined cards of a payment and of its customer, read
// from the try history before the snapshot's transaction. The history is
// observational: when it cannot be read, known is false and the card-count
// rules are skipped rather than stopping the payment.
type declineHistory struct {
	payment, customer map[string]int
	known             bool
}

func (s *Service) loadDeclineHistory(ctx context.Context, attemptID string) declineHistory {
	var h declineHistory
	err := s.db.Read(ctx, func(q store.Querier) error {
		a, err := store.GetAttempt(ctx, q, attemptID)
		if err != nil {
			return err
		}
		p, err := store.GetPayment(ctx, q, a.PaymentID)
		if err != nil {
			return err
		}
		if h.payment, err = store.DeclinedCards(ctx, q, p.ID); err != nil {
			return err
		}
		if p.CustomerID != "" {
			h.customer, err = store.CustomerDeclinedCards(ctx, q, p.CustomerID)
		}
		return err
	})
	if err != nil {
		slog.Warn("core: declined cards could not be read; card-testing limits skipped", "attempt", attemptID, "err", err)
		return declineHistory{}
	}
	h.known = true
	return h
}

// reviewReason says why a success on p must go to review, or "" when it may
// succeed. The caller holds the payment lock.
func (s *Service) reviewReason(ctx context.Context, tx store.Tx, p store.Payment, snap Snapshot, hist declineHistory, ipc ipCheck) (string, error) {
	if merchantInitiated(p) {
		return "", nil
	}
	if p.HasFlag(FlagCardTesting) {
		return FlagCardTesting, nil
	}
	var c store.Customer
	if p.CustomerID != "" {
		var err error
		c, err = store.GetCustomer(ctx, tx, p.CustomerID)
		if err != nil && err != store.ErrNotFound {
			return "", err
		}
		if !c.BlockedAt.IsZero() {
			return FlagCustomerBlocked, nil
		}
	}
	if hist.known {
		limit, err := s.intPolicy(ctx, tx, p.AppID, "review_after_declined_cards")
		if err != nil {
			return "", err
		}
		others := len(hist.payment)
		if hist.payment[snapCardKey(snap)] > 0 {
			others-- // a card declined once and then accepted is not another card
		}
		if limit > 0 && others >= limit {
			return FlagDeclinedCards, nil
		}
	}
	if reason, err := s.ipReason(ctx, tx, p, ipc); reason != "" || err != nil {
		return reason, err
	}
	return s.addressReason(ctx, tx, p, c, snap)
}

// guardDeclines applies the card-testing limits after a declined try on p:
// the payment stops taking attempts at card_decline_limit different cards, and
// its customer is blocked at customer_decline_limit across their payments.
// The caller holds the payment lock; p is updated in place.
func (s *Service) guardDeclines(ctx context.Context, tx store.Tx, p *store.Payment, attemptID string, hist declineHistory, alerts *[]notify.Alert) error {
	if !hist.known {
		return nil
	}
	now := s.nowUTC()
	if !p.HasFlag(FlagCardTesting) && !merchantInitiated(*p) {
		limit, err := s.intPolicy(ctx, tx, p.AppID, "card_decline_limit")
		if err != nil {
			return err
		}
		if cards := hist.payment; limit > 0 {
			if len(cards) >= limit {
				addFlag(p, FlagCardTesting)
				if err := store.UpdatePayment(ctx, tx, p, now); err != nil {
					return err
				}
				msg := fmt.Sprintf("Payment %s stopped taking new attempts: %d different cards were declined on it. A success on it goes to review.", p.ID, len(cards))
				if p.Status == string(state.PaymentSucceeded) {
					// Cards tested on a checkout that already paid: the card
					// that paid is likely stolen too.
					msg = fmt.Sprintf("Payment %s already succeeded, and then %d different cards were declined on its checkout: card testing. The card that paid may be stolen too; check the payment and refund it before the cardholder disputes it.", p.ID, len(cards))
				}
				*alerts = append(*alerts, notify.Alert{Severity: notify.SeverityWarning, Kind: "payment.card_testing",
					Message: msg,
					Refs:    map[string]string{"payment": p.ID, "attempt": attemptID, "app": p.AppID}})
			}
		}
	}
	if p.CustomerID == "" {
		return nil
	}
	limit, err := s.intPolicy(ctx, tx, p.AppID, "customer_decline_limit")
	if err != nil || limit <= 0 {
		return err
	}
	c, err := store.GetCustomer(ctx, tx, p.CustomerID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil
		}
		return err
	}
	if !c.BlockedAt.IsZero() {
		return nil
	}
	cards := hist.customer
	if len(cards) < limit {
		return nil
	}
	if _, err := store.SetCustomerBlock(ctx, tx, c.ID, BlockReasonDeclinedCards, now); err != nil {
		return err
	}
	if err := audit.Record(ctx, tx, audit.Entry{OrgID: c.OrgID, Actor: audit.System, Action: "customer.block",
		Target: c.ID, After: map[string]any{"reason": BlockReasonDeclinedCards, "declined_cards": len(cards), "payment": p.ID}}); err != nil {
		return err
	}
	*alerts = append(*alerts, notify.Alert{Severity: notify.SeverityWarning, Kind: "customer.blocked",
		Message: fmt.Sprintf("Customer %s was blocked from paying: %d different cards were declined across their payments (latest on payment %s).", c.ID, len(cards), p.ID),
		Refs:    map[string]string{"customer": c.ID, "payment": p.ID, "app": p.AppID}})
	return nil
}

// refusedForCardTesting says why p takes no new attempt, or "" when it may.
func refusedForCardTesting(ctx context.Context, tx store.Tx, p store.Payment) (string, error) {
	if merchantInitiated(p) {
		return "", nil
	}
	if p.HasFlag(FlagCardTesting) {
		return "the payment stopped taking attempts after too many declined cards", nil
	}
	if p.CustomerID == "" {
		return "", nil
	}
	c, err := store.GetCustomer(ctx, tx, p.CustomerID)
	if err != nil {
		if err == store.ErrNotFound {
			return "", nil
		}
		return "", err
	}
	if !c.BlockedAt.IsZero() {
		return "the customer is blocked from paying", nil
	}
	return "", nil
}

// Approvable reports whether ApproveReview may move p: it is held for a
// card-testing review, not an amount or cancellation one.
func Approvable(p store.Payment) bool {
	return p.Status == string(state.PaymentNeedsReview) && slices.ContainsFunc(reviewReasons, p.HasFlag)
}

// ApproveReview moves a payment held for a card-testing review to succeeded,
// on an operator's word, and emits payment.succeeded. It is refused for any
// other review (an amount mismatch, a success after cancel), which an operator
// settles with a refund.
func (s *Service) ApproveReview(ctx context.Context, actor audit.Actor, paymentID string) (store.Payment, error) {
	var p store.Payment
	var alerts []notify.Alert
	err := s.db.Write(ctx, func(tx store.Tx) error {
		alerts = nil
		var err error
		if p, err = store.GetPaymentForUpdate(ctx, tx, paymentID); err != nil {
			return mapStoreErr(err, "payment")
		}
		if !Approvable(p) {
			return newErr(ErrInvalidState, "only a payment held for a declined-cards review can be approved")
		}
		attempts, err := store.ListAttempts(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		var paid store.Attempt
		for _, a := range attempts {
			if a.Status == string(state.AttemptCaptured) && !a.Duplicate {
				paid = a
			}
		}
		if paid.ID == "" || paid.AmountReceived != p.Amount || paid.CurrencyReceived != p.Currency {
			return newErr(ErrInvalidState, "the payment has no captured attempt for its full amount")
		}
		before := ViewPayment(p)
		p.Status = string(state.PaymentSucceeded)
		now := s.nowUTC()
		if err := store.UpdatePayment(ctx, tx, &p, now); err != nil {
			return err
		}
		if err := s.emitPayment(ctx, tx, "payment.succeeded", p, "", paid.ID); err != nil {
			return err
		}
		if s.onPaymentSucceeded != nil {
			if err := s.onPaymentSucceeded(context.WithValue(ctx, alertSinkKey{}, &alerts), tx, p, paid); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: p.OrgID, Actor: actor, Action: "payment.approve",
			Target: p.ID, Before: before, After: ViewPayment(p)})
	})
	if err == nil {
		s.raise(ctx, alerts)
	}
	return p, err
}

// SetCustomerBlocked blocks (blocked true) or unblocks a customer on an
// operator's word. It reports whether anything changed.
func (s *Service) SetCustomerBlocked(ctx context.Context, actor audit.Actor, customerID string, blocked bool) (bool, error) {
	var changed bool
	err := s.db.Write(ctx, func(tx store.Tx) error {
		c, err := store.GetCustomer(ctx, tx, customerID)
		if err != nil {
			return mapStoreErr(err, "customer")
		}
		reason, action := "", "customer.unblock"
		if blocked {
			reason, action = BlockReasonOperator, "customer.block"
		}
		if changed, err = store.SetCustomerBlock(ctx, tx, c.ID, reason, s.nowUTC()); err != nil || !changed {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: c.OrgID, Actor: actor, Action: action, Target: c.ID,
			Before: map[string]any{"blocked": !blocked, "reason": c.BlockedReason}, After: map[string]any{"blocked": blocked, "reason": reason}})
	})
	return changed, err
}

// FlagFraud is an operator's verdict that a payment is fraud: a stolen card,
// card testing, a fraud alert from the provider. It changes nothing else on
// the payment; refund it and block the customer as needed.
const FlagFraud = "fraud"

// SetFraud tags (fraud true) or untags payment paymentID as fraud on an
// operator's word. It reports whether anything changed.
func (s *Service) SetFraud(ctx context.Context, actor audit.Actor, paymentID string, fraud bool) (bool, error) {
	var changed bool
	err := s.db.Write(ctx, func(tx store.Tx) error {
		changed = false
		p, err := store.GetPaymentForUpdate(ctx, tx, paymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		if p.HasFlag(FlagFraud) == fraud {
			return nil
		}
		before := ViewPayment(p)
		action := "payment.fraud_unmark"
		if fraud {
			addFlag(&p, FlagFraud)
			action = "payment.fraud_mark"
		} else {
			p.Flags = slices.DeleteFunc(p.Flags, func(f string) bool { return f == FlagFraud })
		}
		if err := store.UpdatePayment(ctx, tx, &p, s.nowUTC()); err != nil {
			return err
		}
		changed = true
		return audit.Record(ctx, tx, audit.Entry{OrgID: p.OrgID, Actor: actor, Action: action, Target: p.ID,
			Before: before, After: ViewPayment(p)})
	})
	return changed, err
}
