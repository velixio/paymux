package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

func terminal(status string) bool {
	switch state.SubscriptionStatus(status) {
	case state.SubCanceled, state.SubEnded, state.SubIncompleteExpired:
		return true
	}
	return false
}

// scoped locks a subscription of the scope and runs fn with it.
func (s *Service) scoped(ctx context.Context, sc core.Scope, id string, fn func(tx store.Tx, sub *store.Subscription) error) error {
	if err := sc.Validate(); err != nil {
		return newErr(ErrInvalid, "%v", err)
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		sub, err := s.lockSub(ctx, tx, id)
		if err != nil {
			return err
		}
		if sub.AppID != sc.AppID || sub.Mode != sc.Mode {
			return newErr(ErrNotFound, "subscription not found")
		}
		return fn(tx, &sub)
	})
}

// transition applies a subscription event. It returns false when the event
// changes nothing (already there, or a terminal state).
func (s *Service) transition(sub *store.Subscription, ev state.SubscriptionEvent) (bool, error) {
	out := state.SubscriptionNext(state.SubscriptionStatus(sub.Status), ev)
	switch out.Action {
	case state.Reject:
		return false, newErr(ErrInvalidState, "subscription cannot %s while %s", ev, sub.Status)
	case state.Ignore:
		return false, nil
	}
	sub.Status = string(out.Next)
	return true, nil
}

func (s *Service) saveSub(ctx context.Context, tx store.Tx, sub *store.Subscription) error {
	if err := store.UpdateSubscription(ctx, tx, sub, s.nowUTC()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return newErr(ErrInvalidState, "subscription changed concurrently")
		}
		return err
	}
	return nil
}

// mandateEvent applies a mandate event and emits the matching mandate.* event.
func (s *Service) mandateEvent(ctx context.Context, tx store.Tx, m *store.Mandate, ev state.MandateEvent) (bool, error) {
	out := state.MandateNext(state.MandateStatus(m.Status), ev)
	if out.Action != state.Apply {
		return false, nil
	}
	m.Status = string(out.Next)
	if err := store.UpdateMandate(ctx, tx, *m, s.nowUTC()); err != nil {
		return false, err
	}
	typ := map[state.MandateEvent]string{state.MandateActivated: EvMandateActivated, state.MandateRevokedEv: EvMandateRevoked,
		state.MandateExpiredEv: EvMandateExpired, state.MandateFailedEv: EvMandateFailed}[ev]
	return true, s.emitMandate(ctx, tx, typ, *m)
}

// ---- cancel ------------------------------------------------------------

// Cancel is CancelBy attributed to the system.
func (s *Service) Cancel(ctx context.Context, sc core.Scope, id string, atPeriodEnd bool) (store.Subscription, error) {
	return s.CancelBy(ctx, sc, id, atPeriodEnd, audit.System)
}

// CancelBy cancels a subscription now, or at the end of the current period. Now
// revokes the mandate or cancels at the provider (through a durable job). A
// cancel that changes something is audited under actor in the same transaction.
func (s *Service) CancelBy(ctx context.Context, sc core.Scope, id string, atPeriodEnd bool, actor audit.Actor) (store.Subscription, error) {
	var out store.Subscription
	err := s.scoped(ctx, sc, id, func(tx store.Tx, sub *store.Subscription) error {
		before := *sub
		defer func() {
			out = *sub
			if sub.Status != before.Status || sub.CancelAtPeriodEnd != before.CancelAtPeriodEnd {
				_ = audit.Record(ctx, tx, audit.Entry{OrgID: sub.OrgID, Actor: actor, Action: "subscription.cancel", Target: sub.ID,
					Before: map[string]any{"status": before.Status, "cancel_at_period_end": before.CancelAtPeriodEnd},
					After:  map[string]any{"status": sub.Status, "cancel_at_period_end": sub.CancelAtPeriodEnd, "at_period_end": atPeriodEnd}})
			}
		}()
		if terminal(sub.Status) {
			if sub.Status == string(state.SubCanceled) {
				return nil // repeating a cancel is harmless
			}
			return newErr(ErrInvalidState, "a %s subscription cannot be canceled", sub.Status)
		}
		if atPeriodEnd && sub.Status != string(state.SubIncomplete) {
			if sub.CancelAtPeriodEnd {
				return nil
			}
			sub.CancelAtPeriodEnd = true
			if err := s.saveSub(ctx, tx, sub); err != nil {
				return err
			}
			if sub.BillingMode == string(connector.ProviderManaged) {
				if err := s.enqueueProviderCancel(ctx, tx, sub, true); err != nil {
					return err
				}
			}
			return s.emitSub(ctx, tx, EvUpdated, *sub, eventData{Reason: "cancel_at_period_end"})
		}
		return s.cancelNow(ctx, tx, sub, "canceled_by_request")
	})
	return out, err
}

func (s *Service) cancelNow(ctx context.Context, tx store.Tx, sub *store.Subscription, reason string) error {
	ok, err := s.transition(sub, state.SubCancel)
	if err != nil || !ok {
		return err
	}
	return s.afterTermination(ctx, tx, sub, EvCanceled, reason)
}

// afterTermination finishes a canceled or ended subscription: stamps the
// time, revokes mandates, cancels at the provider and emits the event.
func (s *Service) afterTermination(ctx context.Context, tx store.Tx, sub *store.Subscription, ev, reason string) error {
	now := s.nowUTC()
	if sub.Status == string(state.SubEnded) {
		sub.EndedAt = now
	} else {
		sub.CanceledAt = now
	}
	sub.CancelAtPeriodEnd, sub.RequiresReauthorization, sub.CheckoutAction = false, false, ""
	sub.PendingAmount, sub.NextRetryAt = 0, time.Time{}
	for _, mid := range []string{sub.MandateID, sub.PendingMandateID} {
		if mid == "" {
			continue
		}
		m, err := store.GetMandateForUpdate(ctx, tx, mid)
		if err != nil {
			return err
		}
		revoked, err := s.mandateEvent(ctx, tx, &m, state.MandateRevokedEv)
		if err != nil {
			return err
		}
		if !revoked && m.Status == string(state.MandatePending) {
			continue
		}
		if m.ProviderMandateID != "" {
			if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobRevokeMandate, Payload: map[string]string{"mandate_id": m.ID},
				DedupeKey: "mandate:" + m.ID + ":revoke"}); err != nil {
				return err
			}
		}
	}
	sub.PendingMandateID = ""
	if sub.BillingMode == string(connector.ProviderManaged) && sub.ProviderSubscriptionID != "" {
		if err := s.enqueueProviderCancel(ctx, tx, sub, false); err != nil {
			return err
		}
	}
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return err
	}
	return s.emitSub(ctx, tx, ev, *sub, eventData{Reason: reason})
}

func (s *Service) enqueueProviderCancel(ctx context.Context, tx store.Tx, sub *store.Subscription, atPeriodEnd bool) error {
	if sub.ProviderSubscriptionID == "" {
		return nil
	}
	key := fmt.Sprintf("sub:%s:provider-cancel:%t", sub.ID, atPeriodEnd)
	return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobCancelAtProvider, DedupeKey: key,
		Payload: map[string]any{"subscription_id": sub.ID, "at_period_end": atPeriodEnd}})
}

// ---- update amount -----------------------------------------------------

// UpdateResult is the result of UpdateAmount.
type UpdateResult struct {
	Subscription store.Subscription
	// CheckoutURL is set when the new amount needs the customer to authorise
	// a larger mandate first (Subscription.RequiresReauthorization).
	CheckoutURL string
}

// UpdateAmount changes the amount from the next period on, with no proration.
// For a mandate whose maximum is below the new amount it starts a
// re-authorization (a new pending mandate) and emits
// subscription.reauthorization_required with a fresh checkout URL.
func (s *Service) UpdateAmount(ctx context.Context, sc core.Scope, id string, amount int64) (UpdateResult, error) {
	if amount <= 0 || amount > maxAmount {
		return UpdateResult{}, newErr(ErrInvalid, "amount must be a positive integer in minor units")
	}
	var res UpdateResult
	var providerID string
	// Provider-managed: tell the provider first (it applies the change at the
	// end of the cycle), then record it.
	pre := func() error {
		return s.db.Read(ctx, func(q store.Querier) error {
			sub, err := store.GetSubscription(ctx, q, id)
			if err != nil || sub.AppID != sc.AppID || sub.Mode != sc.Mode {
				return newErr(ErrNotFound, "subscription not found")
			}
			if sub.BillingMode == string(connector.ProviderManaged) && !terminal(sub.Status) && sub.Status != string(state.SubIncomplete) &&
				sub.Amount != amount {
				providerID = sub.ProviderAccountID
			}
			return nil
		})
	}
	if err := pre(); err != nil {
		return UpdateResult{}, err
	}
	if providerID != "" {
		var psid string
		_ = s.db.Read(ctx, func(q store.Querier) error {
			sub, _ := store.GetSubscription(ctx, q, id)
			psid = sub.ProviderSubscriptionID
			return nil
		})
		if psid == "" {
			return UpdateResult{}, newErr(ErrInvalidState, "the subscription has no provider subscription yet")
		}
		conn, err := s.connectors.Get(ctx, providerID)
		if err != nil {
			return UpdateResult{}, err
		}
		mgr, ok := conn.(connector.SubscriptionManager)
		if !ok {
			return UpdateResult{}, newErr(ErrUnsupported, "the provider cannot change subscription amounts")
		}
		if err := mgr.UpdateSubscriptionAmount(ctx, connector.SubscriptionRef{ProviderSubscriptionID: psid}, amount); err != nil {
			return UpdateResult{}, err
		}
	}
	err := s.scoped(ctx, sc, id, func(tx store.Tx, sub *store.Subscription) error {
		defer func() { res.Subscription = *sub }()
		if terminal(sub.Status) || sub.Status == string(state.SubIncomplete) {
			return newErr(ErrInvalidState, "the amount of a %s subscription cannot be changed", sub.Status)
		}
		if amount == sub.Amount && sub.PendingAmount == 0 {
			return nil
		}
		if amount == sub.Amount {
			sub.PendingAmount = 0
			return s.saveSub(ctx, tx, sub)
		}
		sub.PendingAmount = amount
		if sub.BillingMode == string(connector.MerchantTriggered) {
			pol, err := s.loadPolicy(ctx, tx, sub.AppID)
			if err != nil {
				return err
			}
			m, err := store.GetMandate(ctx, tx, sub.MandateID)
			if err != nil {
				return err
			}
			lim, err := s.mandateLimit(ctx, pol, sub.ProviderAccountID, m.Method)
			if err != nil {
				return err
			}
			if amount > lim {
				return newErr(ErrAmountExceedsMandateLimit, "amount %d exceeds the %d a %s mandate allows", amount, lim, m.Method)
			}
			if amount > m.MaxAmount {
				url, err := s.requestReauthorization(ctx, tx, sub, pol, "amount_above_mandate_max")
				if err != nil {
					return err
				}
				res.CheckoutURL = url
				return nil
			}
		}
		if err := s.saveSub(ctx, tx, sub); err != nil {
			return err
		}
		return s.emitSub(ctx, tx, EvUpdated, *sub, eventData{Reason: "amount_changed"})
	})
	return res, err
}

// mandateLimit is the largest debit for a method on a provider account:
// the regulatory limit capped by what the connector declares.
func (s *Service) mandateLimit(ctx context.Context, pol policy, accountID, method string) (int64, error) {
	lim := pol.methodLimit(method)
	conn, err := s.connectors.Get(ctx, accountID)
	if err != nil {
		return 0, err
	}
	if cl := connectorLimit(conn.Info(), method); cl > 0 && cl < lim {
		lim = cl
	}
	return lim, nil
}

// newPendingMandate creates a pending mandate for sub with the given maximum,
// replacing any earlier pending one, and issues a fresh checkout token.
func (s *Service) newPendingMandate(ctx context.Context, tx store.Tx, sub *store.Subscription, pol policy, method string, amount int64) (store.Mandate, string, error) {
	now := s.nowUTC()
	if sub.PendingMandateID != "" {
		old, err := store.GetMandateForUpdate(ctx, tx, sub.PendingMandateID)
		if err == nil {
			if _, err := s.mandateEvent(ctx, tx, &old, state.MandateExpiredEv); err != nil {
				return store.Mandate{}, "", err
			}
		}
	}
	lim, err := s.mandateLimit(ctx, pol, sub.ProviderAccountID, method)
	if err != nil {
		return store.Mandate{}, "", err
	}
	mx, err := billing.MandateMax(amount, pol.headroomBP, lim)
	if err != nil {
		return store.Mandate{}, "", newErr(ErrAmountExceedsMandateLimit, "amount %d exceeds the %d a %s mandate allows", amount, lim, method)
	}
	m := store.Mandate{ID: ids.New("mdt"), OrgID: sub.OrgID, Mode: sub.Mode, AppID: sub.AppID, CustomerID: sub.CustomerID,
		ProviderAccountID: sub.ProviderAccountID, ProviderRef: ids.ProviderRef(), Method: method, Status: "pending",
		MaxAmount: mx, Currency: sub.Currency, Frequency: sub.Interval, ValidUntil: now.AddDate(pol.validityYears, 0, 0),
		CreatedAt: now, UpdatedAt: now}
	if err := store.InsertMandate(ctx, tx, m); err != nil {
		return store.Mandate{}, "", err
	}
	token, hash := newToken()
	sub.PendingMandateID, sub.CheckoutTokenHash, sub.CheckoutAction = m.ID, hash, ""
	return m, token, s.emitMandate(ctx, tx, EvMandateCreated, m)
}

// requestReauthorization starts a re-authorization of sub: a new pending
// mandate sized for its (pending) amount, RequiresReauthorization set, and
// subscription.reauthorization_required emitted with the checkout URL. It
// returns that URL ("" when no mandate can cover the amount).
func (s *Service) requestReauthorization(ctx context.Context, tx store.Tx, sub *store.Subscription, pol policy, reason string) (string, error) {
	cur, err := store.GetMandate(ctx, tx, sub.MandateID)
	if err != nil {
		return "", err
	}
	amount := sub.Amount
	if sub.PendingAmount > amount {
		amount = sub.PendingAmount
	}
	var url string
	m, token, err := s.newPendingMandate(ctx, tx, sub, pol, cur.Method, amount)
	switch {
	case err == nil:
		url = s.checkoutURL(sub.ID, token)
		_ = m
	case errors.Is(err, ErrAmountExceedsMandateLimit):
		reason += ":amount_exceeds_mandate_limit"
	default:
		return "", err
	}
	sub.RequiresReauthorization = true
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return "", err
	}
	return url, s.emitSub(ctx, tx, EvReauthorizationNeeded, *sub, eventData{Reason: reason, CheckoutURL: url})
}

// ---- payment method ----------------------------------------------------

// UpdatePaymentMethod starts a new mandate checkout for a merchant-triggered
// subscription; the old mandate is revoked once the new one is active.
// method is optional (default: the current mandate's method).
func (s *Service) UpdatePaymentMethod(ctx context.Context, sc core.Scope, id, method string) (Created, error) {
	var out Created
	err := s.scoped(ctx, sc, id, func(tx store.Tx, sub *store.Subscription) error {
		if terminal(sub.Status) || sub.Status == string(state.SubIncomplete) {
			return newErr(ErrInvalidState, "the payment method of a %s subscription cannot be changed", sub.Status)
		}
		if sub.BillingMode != string(connector.MerchantTriggered) {
			// TODO(spec): provider-managed payment-method updates need the
			// provider's own update link; Razorpay offers none through the API.
			return newErr(ErrUnsupported, "this subscription's payment method is managed at the provider")
		}
		pol, err := s.loadPolicy(ctx, tx, sub.AppID)
		if err != nil {
			return err
		}
		cur, err := store.GetMandate(ctx, tx, sub.MandateID)
		if err != nil {
			return err
		}
		if method == "" {
			method = cur.Method
		}
		amount := max(sub.Amount, sub.PendingAmount)
		_, token, err := s.newPendingMandate(ctx, tx, sub, pol, method, amount)
		if err != nil {
			return err
		}
		if err := s.saveSub(ctx, tx, sub); err != nil {
			return err
		}
		out = Created{Subscription: *sub, CheckoutURL: s.checkoutURL(sub.ID, token)}
		return s.emitSub(ctx, tx, EvUpdated, *sub, eventData{Reason: "payment_method_change_started", CheckoutURL: out.CheckoutURL})
	})
	return out, err
}

// IssueCheckoutLink issues a new checkout link for a subscription that still
// needs the customer: an incomplete one, or a merchant-triggered one waiting
// for a (re-)authorization. Tokens are stored hashed, so an earlier link cannot
// be shown again; this one replaces it (the old one stops working). A running
// subscription whose re-authorization lapsed gets a new pending mandate. The
// action is audited under actor (without the token).
func (s *Service) IssueCheckoutLink(ctx context.Context, sc core.Scope, id string, actor audit.Actor) (Created, error) {
	var out Created
	err := s.scoped(ctx, sc, id, func(tx store.Tx, sub *store.Subscription) error {
		if terminal(sub.Status) {
			return newErr(ErrInvalidState, "a %s subscription has no checkout", sub.Status)
		}
		var token string
		switch {
		case sub.Status == string(state.SubIncomplete) || sub.PendingMandateID != "":
			var hash string
			token, hash = newToken()
			sub.CheckoutTokenHash = hash
		case reauthRetry(*sub):
			pol, err := s.loadPolicy(ctx, tx, sub.AppID)
			if err != nil {
				return err
			}
			cur, err := store.GetMandate(ctx, tx, sub.MandateID)
			if err != nil {
				return err
			}
			if _, token, err = s.newPendingMandate(ctx, tx, sub, pol, cur.Method, max(sub.Amount, sub.PendingAmount)); err != nil {
				return err
			}
		default:
			return newErr(ErrInvalidState, "this subscription is not waiting for the customer")
		}
		if err := s.saveSub(ctx, tx, sub); err != nil {
			return err
		}
		out = Created{Subscription: *sub, CheckoutURL: s.checkoutURL(sub.ID, token)}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sub.OrgID, Actor: actor, Action: "subscription.checkout_link", Target: sub.ID,
			After: map[string]any{"status": sub.Status, "requires_reauthorization": sub.RequiresReauthorization}})
	})
	return out, err
}
