package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/checkout"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// ---- expiry and provider jobs -------------------------------------------

// handleExpireIncomplete expires a subscription whose setup was not completed
// within 24 hours.
func (s *Service) handleExpireIncomplete(ctx context.Context, raw json.RawMessage) error {
	var pl struct {
		SubscriptionID string `json:"subscription_id"`
	}
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		sub, err := s.lockSub(ctx, tx, pl.SubscriptionID)
		if err != nil {
			return err
		}
		if sub.Status != string(state.SubIncomplete) || s.nowUTC().Before(sub.ExpiresAt) {
			return nil
		}
		if _, err := s.transition(&sub, state.SubSetupExpired); err != nil {
			return err
		}
		sub.CheckoutAction = ""
		for _, mid := range []string{sub.MandateID, sub.PendingMandateID} {
			if mid == "" {
				continue
			}
			m, err := store.GetMandateForUpdate(ctx, tx, mid)
			if err != nil {
				return err
			}
			if _, err := s.mandateEvent(ctx, tx, &m, state.MandateExpiredEv); err != nil {
				return err
			}
		}
		if sub.BillingMode == string(connector.ProviderManaged) {
			if err := s.enqueueProviderCancel(ctx, tx, &sub, false); err != nil {
				return err
			}
		}
		if err := s.saveSub(ctx, tx, &sub); err != nil {
			return err
		}
		return s.emitSub(ctx, tx, EvIncompleteExpired, sub, eventData{})
	})
}

func (s *Service) handleRevokeMandate(ctx context.Context, raw json.RawMessage) error {
	var pl struct {
		MandateID string `json:"mandate_id"`
	}
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var m store.Mandate
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { m, err = store.GetMandate(ctx, q, pl.MandateID); return }); err != nil {
		return err
	}
	if m.ProviderMandateID == "" {
		return nil
	}
	conn, err := s.connectors.Get(ctx, m.ProviderAccountID)
	if err != nil {
		return err
	}
	mm, ok := conn.(connector.MandateManager)
	if !ok {
		return jobs.Permanent(errors.New("connector cannot revoke mandates"))
	}
	err = mm.RevokeMandate(ctx, connector.MandateRef{ProviderMandateID: m.ProviderMandateID})
	if errors.Is(err, connector.ErrNotFound) {
		return nil
	}
	if errors.Is(err, connector.ErrUnsupported) {
		// The provider has no API to end a mandate (Zoho: the customer cancels in
		// their UPI app). Paymux has stopped presenting debits; nothing to retry.
		return nil
	}
	return err
}

func (s *Service) handleCancelAtProvider(ctx context.Context, raw json.RawMessage) error {
	var pl struct {
		SubscriptionID string `json:"subscription_id"`
		AtPeriodEnd    bool   `json:"at_period_end"`
	}
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var sub store.Subscription
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { sub, err = store.GetSubscription(ctx, q, pl.SubscriptionID); return }); err != nil {
		return err
	}
	if sub.ProviderSubscriptionID == "" {
		return nil
	}
	conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
	if err != nil {
		return err
	}
	mgr, ok := conn.(connector.SubscriptionManager)
	if !ok {
		return jobs.Permanent(errors.New("connector cannot cancel subscriptions"))
	}
	err = mgr.CancelSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sub.ProviderSubscriptionID}, pl.AtPeriodEnd)
	if errors.Is(err, connector.ErrNotFound) {
		return nil
	}
	return err
}

// scheduleMandateExpiry enqueues the 30-day warning and the expiry itself.
func (s *Service) scheduleMandateExpiry(ctx context.Context, tx store.Tx, m store.Mandate) error {
	if m.ValidUntil.IsZero() {
		return nil
	}
	now := s.nowUTC()
	for _, p := range []struct {
		phase string
		at    time.Time
	}{{"warn", m.ValidUntil.AddDate(0, 0, -30)}, {"expire", m.ValidUntil}} {
		at := p.at
		if at.Before(now) {
			at = now
		}
		if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobMandateExpiry, RunAt: at,
			Payload: map[string]string{"mandate_id": m.ID, "phase": p.phase}, DedupeKey: "mandate:" + m.ID + ":" + p.phase}); err != nil {
			return err
		}
	}
	return nil
}

// handleMandateExpiry warns 30 days before a mandate expires (a
// re-authorization is requested for every subscription on it) and expires it
// on its valid_until date.
func (s *Service) handleMandateExpiry(ctx context.Context, raw json.RawMessage) error {
	var pl struct {
		MandateID string `json:"mandate_id"`
		Phase     string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		subs, err := s.lockMandateSubs(ctx, tx, pl.MandateID)
		if err != nil {
			return err
		}
		m, err := store.GetMandateForUpdate(ctx, tx, pl.MandateID)
		if err != nil {
			return err
		}
		if m.Status != string(state.MandateActive) {
			return nil
		}
		now := s.nowUTC()
		switch pl.Phase {
		case "warn":
			if !m.ExpiryNotifiedAt.IsZero() {
				return nil
			}
			m.ExpiryNotifiedAt = now
			if err := store.UpdateMandate(ctx, tx, m, now); err != nil {
				return err
			}
			for i := range subs {
				sub := &subs[i]
				if sub.MandateID != m.ID || terminal(sub.Status) || sub.RequiresReauthorization {
					continue
				}
				pol, err := s.loadPolicy(ctx, tx, sub.AppID)
				if err != nil {
					return err
				}
				if _, err := s.requestReauthorization(ctx, tx, sub, pol, "mandate_expiring"); err != nil {
					return err
				}
			}
		case "expire":
			if m.ValidUntil.After(now) {
				return nil
			}
			return s.applyMandateEvent(ctx, tx, &m, subs, state.MandateExpiredEv, nil)
		}
		return nil
	})
}

// lockMandateSubs locks every subscription using a mandate, in id order,
// before the mandate itself is locked (lock order: subscription, mandate).
func (s *Service) lockMandateSubs(ctx context.Context, tx store.Tx, mandateID string) ([]store.Subscription, error) {
	found, err := store.SubscriptionsByMandate(ctx, tx, mandateID)
	if err != nil {
		return nil, err
	}
	out := make([]store.Subscription, 0, len(found))
	for _, f := range found {
		sub, err := s.lockSub(ctx, tx, f.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
}

// ---- mandate events -------------------------------------------------------

// applyMandateEvent applies a mandate event and its consequences for the
// subscriptions that use the mandate. subs must be locked by the caller.
func (s *Service) applyMandateEvent(ctx context.Context, tx store.Tx, m *store.Mandate, subs []store.Subscription, ev state.MandateEvent, setup *connector.PaymentSnapshot) error {
	changed, err := s.mandateEvent(ctx, tx, m, ev)
	if err != nil || !changed {
		return err
	}
	for i := range subs {
		sub := &subs[i]
		if terminal(sub.Status) {
			continue
		}
		pol, err := s.loadPolicy(ctx, tx, sub.AppID)
		if err != nil {
			return err
		}
		switch ev {
		case state.MandateActivated:
			if err := s.scheduleMandateExpiry(ctx, tx, *m); err != nil {
				return err
			}
			switch {
			case sub.PendingMandateID == m.ID:
				if err := s.swapMandate(ctx, tx, sub, m); err != nil {
					return err
				}
				if err := s.bookVerification(ctx, tx, *sub, setup); err != nil {
					return err
				}
			case sub.MandateID == m.ID && sub.Status == string(state.SubIncomplete):
				if err := s.activate(ctx, tx, sub); err != nil {
					return err
				}
				if err := s.startBilling(ctx, tx, sub, setup); err != nil {
					return err
				}
			}
		case state.MandateFailedEv:
			if sub.PendingMandateID == m.ID {
				sub.PendingMandateID, sub.CheckoutAction = "", ""
				if err := s.saveSub(ctx, tx, sub); err != nil {
					return err
				}
			}
		case state.MandateRevokedEv, state.MandateExpiredEv:
			if sub.MandateID == m.ID && sub.Status != string(state.SubIncomplete) {
				reason := "mandate_revoked"
				if ev == state.MandateExpiredEv {
					reason = "mandate_expired"
				}
				if _, err := s.requestReauthorization(ctx, tx, sub, pol, reason); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// swapMandate makes the freshly activated pending mandate the subscription's
// mandate, revokes the old one and resumes a renewal that was waiting for it.
func (s *Service) swapMandate(ctx context.Context, tx store.Tx, sub *store.Subscription, nm *store.Mandate) error {
	oldID := sub.MandateID
	sub.MandateID, sub.PendingMandateID = nm.ID, ""
	sub.RequiresReauthorization, sub.CheckoutAction = false, ""
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return err
	}
	if oldID != "" && oldID != nm.ID {
		old, err := store.GetMandateForUpdate(ctx, tx, oldID)
		if err != nil {
			return err
		}
		if _, err := s.mandateEvent(ctx, tx, &old, state.MandateRevokedEv); err != nil {
			return err
		}
		if old.ProviderMandateID != "" {
			if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobRevokeMandate, Payload: map[string]string{"mandate_id": old.ID},
				DedupeKey: "mandate:" + old.ID + ":revoke"}); err != nil {
				return err
			}
		}
	}
	// The next debit is on a different mandate than any notice announced, so
	// it needs a fresh notice (RBI): reset the latest unpaid renewal and
	// announce it again. A renewal that failed for want of a usable mandate is
	// resumed, but not before 24h after that notice.
	cys, err := store.ListCycles(ctx, tx, sub.ID)
	if err != nil {
		return err
	}
	now := s.nowUTC()
	for i := len(cys) - 1; i >= 0; i-- {
		cy := cys[i]
		if cy.PeriodIndex < 0 || (cy.State != "scheduled" && cy.State != "failed" && cy.State != "exhausted") {
			continue
		}
		resumed := cy.State != "scheduled"
		cy.State, cy.NotifiedAt = "scheduled", time.Time{}
		if resumed {
			cy.Retries = 0
			if err := s.enqueueCharge(ctx, tx, *sub, &cy, now.Add(billing.MinNoticeGap)); err != nil {
				return err
			}
		}
		if err := s.enqueueNotifyStep(ctx, tx, *sub, cy.PeriodIndex, "notify-mandate-"+nm.ID, now); err != nil {
			return err
		}
		if err := store.UpdateCycle(ctx, tx, cy, now); err != nil {
			return err
		}
		break
	}
	return s.emitSub(ctx, tx, EvUpdated, *sub, eventData{Reason: "mandate_replaced"})
}

func mandateEventFor(status string) (state.MandateEvent, bool) {
	switch status {
	case "active":
		return state.MandateActivated, true
	case "failed":
		return state.MandateFailedEv, true
	case "revoked":
		return state.MandateRevokedEv, true
	case "expired":
		return state.MandateExpiredEv, true
	}
	return "", false
}

// applyMandateSnapshot applies a provider's view of a mandate.
func (s *Service) applyMandateSnapshot(ctx context.Context, mandateID string, snap connector.MandateSnapshot) error {
	err := s.db.Write(ctx, func(tx store.Tx) error {
		subs, err := s.lockMandateSubs(ctx, tx, mandateID)
		if err != nil {
			return err
		}
		m, err := store.GetMandateForUpdate(ctx, tx, mandateID)
		if err != nil {
			return mapNotFound(err, "mandate")
		}
		if m.ProviderMandateID == "" && snap.ProviderMandateID != "" {
			m.ProviderMandateID = snap.ProviderMandateID
			if err := store.UpdateMandate(ctx, tx, m, s.nowUTC()); err != nil {
				return err
			}
		}
		if snap.ValidUntil > 0 && m.ValidUntil.UnixMilli() != snap.ValidUntil {
			m.ValidUntil = time.UnixMilli(snap.ValidUntil).UTC()
		}
		ev, ok := mandateEventFor(snap.Status)
		if !ok {
			return nil
		}
		return s.applyMandateEvent(ctx, tx, &m, subs, ev, snap.SetupPayment)
	})
	if err != nil {
		return err
	}
	if err := s.settleSetup(ctx, mandateID, snap.SetupPayment); err != nil {
		return err
	}
	return s.settleVerification(ctx, mandateID, snap.SetupPayment)
}

// ---- provider notifications (inbound glue) --------------------------------

// HandleNotification applies a parsed provider notification about a
// subscription or a mandate. The inbound processing job calls it for
// notifications of kind subscription and mandate; others are ignored. A
// subscription.charged notification carries the renewal payment, which becomes
// a `renewal` Payment with a captured Attempt (deduplicated per provider
// payment id). It is idempotent.
func (s *Service) HandleNotification(ctx context.Context, providerAccountID string, n connector.Notification) error {
	err := s.handleNotification(ctx, providerAccountID, n)
	if errors.Is(err, ErrNotFound) {
		// An object we do not know: the payment flow records it as unknown.
		return fmt.Errorf("%w: %w", checkout.ErrUnknownObject, err)
	}
	return err
}

func (s *Service) handleNotification(ctx context.Context, providerAccountID string, n connector.Notification) error {
	switch {
	case n.Subscription != nil:
		return s.onSubscription(ctx, providerAccountID, *n.Subscription, n.Payment)
	case n.Mandate != nil:
		var m store.Mandate
		err := s.db.Read(ctx, func(q store.Querier) (err error) {
			m, err = store.GetMandateByProviderID(ctx, q, providerAccountID, n.Mandate.ProviderMandateID)
			return
		})
		if err != nil {
			return mapNotFound(err, "mandate")
		}
		return s.applyMandateSnapshot(ctx, m.ID, *n.Mandate)
	}
	return nil
}

func (s *Service) onSubscription(ctx context.Context, accountID string, snap connector.SubscriptionSnapshot, pay *connector.PaymentSnapshot) error {
	var subID string
	err := s.db.Write(ctx, func(tx store.Tx) error {
		found, err := store.GetSubscriptionByProviderID(ctx, tx, accountID, snap.ProviderSubscriptionID)
		linked := false
		if errors.Is(err, store.ErrNotFound) && snap.ProviderRef != "" {
			// A provider that creates the subscription after the checkout (Whop
			// memberships) names it by our reference; remember its id.
			found, err = store.GetSubscriptionByProviderRef(ctx, tx, snap.ProviderRef)
			if err == nil && found.ProviderAccountID != accountID {
				err = store.ErrNotFound
			}
			linked = err == nil
		}
		if err != nil {
			return mapNotFound(err, "subscription")
		}
		subID = found.ID
		sub, err := s.lockSub(ctx, tx, found.ID)
		if err != nil {
			return err
		}
		if linked && sub.ProviderSubscriptionID != snap.ProviderSubscriptionID && snap.ProviderSubscriptionID != "" {
			sub.ProviderSubscriptionID = snap.ProviderSubscriptionID
			if err := s.saveSub(ctx, tx, &sub); err != nil {
				return err
			}
		}
		return s.applyProviderStatus(ctx, tx, &sub, snap)
	})
	if err != nil {
		return err
	}
	if pay != nil && pay.Status == connector.PaymentSucceeded {
		return s.recordProviderRenewal(ctx, subID, *pay)
	}
	return nil
}

// applyProviderStatus mirrors the provider's subscription status.
func (s *Service) applyProviderStatus(ctx context.Context, tx store.Tx, sub *store.Subscription, snap connector.SubscriptionSnapshot) error {
	if terminal(sub.Status) {
		return nil
	}
	now := s.nowUTC()
	switch snap.Status {
	case "active":
		if sub.Status == string(state.SubIncomplete) {
			return s.activateManaged(ctx, tx, sub, snap)
		}
	case "incomplete":
		// authenticated with a start in the future is a trial.
		if sub.Status == string(state.SubIncomplete) && snap.RawStatus == "authenticated" && sub.TrialEnd.After(now) {
			return s.activateManaged(ctx, tx, sub, snap)
		}
	case "past_due":
		if sub.Status == string(state.SubActive) || sub.Status == string(state.SubTrialing) {
			if _, err := s.transition(sub, state.SubRenewalFailed); err != nil {
				return err
			}
			if err := s.saveSub(ctx, tx, sub); err != nil {
				return err
			}
			return s.emitSub(ctx, tx, EvPastDue, *sub, eventData{Reason: snap.RawStatus})
		}
	case "canceled":
		ok, err := s.transition(sub, state.SubCancel)
		if err != nil || !ok {
			return err
		}
		return s.afterTermination(ctx, tx, sub, EvCanceled, "canceled_at_provider")
	case "ended":
		ok, err := s.transition(sub, state.SubCompleted)
		if err != nil || !ok {
			return err
		}
		return s.afterTermination(ctx, tx, sub, EvEnded, "completed")
	case "incomplete_expired":
		ok, err := s.transition(sub, state.SubSetupExpired)
		if err != nil || !ok {
			return err
		}
		if err := s.saveSub(ctx, tx, sub); err != nil {
			return err
		}
		return s.emitSub(ctx, tx, EvIncompleteExpired, *sub, eventData{})
	}
	return nil
}

func (s *Service) activateManaged(ctx context.Context, tx store.Tx, sub *store.Subscription, snap connector.SubscriptionSnapshot) error {
	if snap.CurrentPeriodStart > 0 && snap.CurrentPeriodEnd > 0 {
		sub.CurrentPeriodStart, sub.CurrentPeriodEnd = time.UnixMilli(snap.CurrentPeriodStart).UTC(), time.UnixMilli(snap.CurrentPeriodEnd).UTC()
	}
	return s.activate(ctx, tx, sub)
}

// recordProviderRenewal books one captured charge of a provider-managed
// subscription as a renewal Payment: payment and attempt are created once per
// provider payment id, then the attempt is captured through the ordinary state
// machine (which runs the invoice hook), then the cycle is settled.
func (s *Service) recordProviderRenewal(ctx context.Context, subID string, ps connector.PaymentSnapshot) error {
	if ps.ProviderPaymentID == "" {
		return newErr(ErrInvalid, "a renewal notification needs a provider payment id")
	}
	var cy store.Cycle
	var sub store.Subscription
	err := s.db.Write(ctx, func(tx store.Tx) error {
		var err error
		if sub, err = s.lockSub(ctx, tx, subID); err != nil {
			return err
		}
		if cy, err = store.GetCycleByProviderPayment(ctx, tx, sub.ID, ps.ProviderPaymentID); err == nil {
			return nil // second delivery: carry on from where the first stopped
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if terminal(sub.Status) {
			return nil
		}
		if sub.Status == string(state.SubIncomplete) {
			if err := s.activate(ctx, tx, &sub); err != nil {
				return err
			}
		}
		cur := ps.Currency
		if cur == "" {
			cur = sub.Currency
		}
		amount := ps.Amount
		if amount == 0 {
			amount = sub.Amount
		}
		p, a, err := s.core.CreateRenewalPayment(ctx, tx, core.RenewalInput{OrgID: sub.OrgID, AppID: sub.AppID, Mode: sub.Mode,
			SubscriptionID: sub.ID, CustomerID: sub.CustomerID, Description: sub.Description, ReturnURL: sub.ReturnURL,
			Metadata: sub.Metadata, Amount: amount, Currency: cur, ProviderAccountID: sub.ProviderAccountID})
		if err != nil {
			return err
		}
		now := s.nowUTC()
		cy = store.Cycle{ID: ids.New("cyc"), SubscriptionID: sub.ID, PeriodIndex: -1, ProviderPaymentID: ps.ProviderPaymentID,
			PaymentID: p.ID, AttemptID: a.ID, Amount: amount, State: "awaiting", CreatedAt: now, UpdatedAt: now}
		return store.InsertCycle(ctx, tx, cy)
	})
	if err != nil || cy.ID == "" {
		return err
	}
	if _, err := s.core.ApplyAttemptSnapshot(ctx, cy.AttemptID, toSnapshot(ps, cy.Amount, sub.Currency)); err != nil {
		return err
	}
	return s.reconcile(ctx, subID, cy.ID)
}

// ReturnRedirect is ApplyReturn for the payment flow: the URL to send the
// customer to ("" when the app has no return URL).
func (s *Service) ReturnRedirect(ctx context.Context, providerAccountID, providerRef string) (string, bool, error) {
	out, handled, err := s.ApplyReturn(ctx, providerAccountID, providerRef)
	return out.ReturnURL, handled, err
}

// ---- hosted checkout return (inbound glue) ----------------------------------

// Outcome is where to send the customer after returning from a provider.
type Outcome struct {
	SubscriptionID string
	Status         string
	ReturnURL      string // the app's return URL with ?subscription_id=...&status=... appended
}

func returnURL(sub store.Subscription) string {
	if sub.ReturnURL == "" {
		return ""
	}
	sep := "?"
	for _, c := range sub.ReturnURL {
		if c == '?' {
			sep = "&"
		}
	}
	return fmt.Sprintf("%s%ssubscription_id=%s&status=%s", sub.ReturnURL, sep, sub.ID, sub.Status)
}

// ApplyReturn handles the customer coming back from the provider after a
// subscription or mandate checkout. providerRef is the reference in the return
// URL (a mandate's or a provider-managed subscription's provider_ref). It
// fetches the provider's truth, applies it, and says where to redirect.
func (s *Service) ApplyReturn(ctx context.Context, providerAccountID, providerRef string) (Outcome, bool, error) {
	var sub store.Subscription
	var m store.Mandate
	isMandate := false
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if m, err = store.GetMandateByRef(ctx, q, providerRef); err == nil {
			isMandate = true
			subs, err := store.SubscriptionsByMandate(ctx, q, m.ID)
			if err == nil && len(subs) > 0 {
				sub = subs[0]
			}
			return err
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		sub, err = store.GetSubscriptionByProviderRef(ctx, q, providerRef)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return Outcome{}, false, nil // not ours: the caller treats it as a payment return
	}
	if err != nil {
		return Outcome{}, false, err
	}
	conn, err := s.connectors.Get(ctx, providerAccountID)
	if err != nil {
		return Outcome{}, true, err
	}
	switch {
	case isMandate:
		mm, ok := conn.(connector.MandateManager)
		if !ok {
			return Outcome{}, true, newErr(ErrUnsupported, "connector does not manage mandates")
		}
		snap, err := mm.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: firstNonEmpty(m.ProviderMandateID, m.ProviderRef)})
		if err != nil {
			return Outcome{}, true, err
		}
		if snap.ProviderMandateID == "" {
			snap.ProviderMandateID = m.ProviderMandateID
		}
		if err := s.applyMandateSnapshot(ctx, m.ID, snap); err != nil {
			return Outcome{}, true, err
		}
	default:
		mgr, ok := conn.(connector.SubscriptionManager)
		if !ok || sub.ProviderSubscriptionID == "" {
			return Outcome{}, true, newErr(ErrInvalidState, "no provider subscription to check")
		}
		snap, err := mgr.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sub.ProviderSubscriptionID})
		switch {
		case errors.Is(err, connector.ErrNotFound):
			// The provider has no subscription yet (the customer has not
			// completed the checkout, or it only appears on its webhook).
		case err != nil:
			return Outcome{}, true, err
		default:
			if err := s.db.Write(ctx, func(tx store.Tx) error {
				locked, err := s.lockSub(ctx, tx, sub.ID)
				if err != nil {
					return err
				}
				return s.applyProviderStatus(ctx, tx, &locked, snap)
			}); err != nil {
				return Outcome{}, true, err
			}
		}
	}
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { sub, err = store.GetSubscription(ctx, q, sub.ID); return }); err != nil {
		return Outcome{}, true, err
	}
	return Outcome{SubscriptionID: sub.ID, Status: sub.Status, ReturnURL: returnURL(sub)}, true, nil
}

// MandatePollWindow is how long a pending mandate is polled: the 24h an
// incomplete subscription may stay incomplete.
const MandatePollWindow = IncompleteTTL

// insertMandate stores a new mandate and, while it is pending, starts polling
// the provider for it: customers do not always come back from the provider's
// page (PayU only completes a registration on the return), so the outcome is
// fetched on the ladder 1m, 2m, 5m, 15m, 1h, 6h, 24h until it settles or the
// window ends.
func (s *Service) insertMandate(ctx context.Context, tx store.Tx, m store.Mandate) error {
	if err := store.InsertMandate(ctx, tx, m); err != nil {
		return err
	}
	if m.Status != string(state.MandatePending) {
		return nil
	}
	return s.enqueueMandatePoll(ctx, tx, m, 0)
}

// enqueueMandatePoll schedules poll number n+1 of the mandate, if it still
// falls inside the window.
func (s *Service) enqueueMandatePoll(ctx context.Context, tx store.Tx, m store.Mandate, n int) error {
	at := s.nowUTC().Add(checkout.PollDelay(n))
	if !at.Before(m.CreatedAt.Add(MandatePollWindow)) {
		return nil
	}
	return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobPollMandate, RunAt: at,
		Payload:   map[string]any{"mandate_id": m.ID, "n": n + 1},
		DedupeKey: fmt.Sprintf("mandate:%s:poll:%d", m.ID, n+1)})
}

// handlePollMandate fetches a pending mandate from the provider and applies
// what it finds; if the provider still has nothing final, the next poll is
// queued.
func (s *Service) handlePollMandate(ctx context.Context, raw json.RawMessage) error {
	var pl struct {
		MandateID string `json:"mandate_id"`
		N         int    `json:"n"`
	}
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var m store.Mandate
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { m, err = store.GetMandate(ctx, q, pl.MandateID); return }); err != nil {
		return err
	}
	if m.Status != string(state.MandatePending) {
		return nil // settled by the return, a webhook, or expiry
	}
	conn, err := s.connectors.Get(ctx, m.ProviderAccountID)
	if err != nil {
		return err
	}
	mm, ok := conn.(connector.MandateManager)
	if !ok {
		return jobs.Permanent(errors.New("connector cannot fetch mandates"))
	}
	snap, err := mm.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: firstNonEmpty(m.ProviderMandateID, m.ProviderRef)})
	switch {
	case errors.Is(err, connector.ErrNotFound):
		snap = connector.MandateSnapshot{Status: "pending"} // the customer has not got that far
	case err != nil:
		return err
	}
	if snap.Status != "pending" {
		return s.applyMandateSnapshot(ctx, m.ID, snap)
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		cur, err := store.GetMandate(ctx, tx, m.ID)
		if err != nil || cur.Status != string(state.MandatePending) {
			return err
		}
		return s.enqueueMandatePoll(ctx, tx, cur, pl.N)
	})
}
