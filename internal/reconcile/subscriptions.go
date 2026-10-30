package reconcile

import (
	"context"
	"errors"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/recon"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// SubscriptionSink applies provider notifications about mandates and
// subscriptions (*subscriptions.Service). Reconciliation hands it the
// provider's current state as a notification, exactly as a webhook would, so
// the subscriptions service stays the only code that changes them.
type SubscriptionSink interface {
	HandleNotification(ctx context.Context, providerAccountID string, n connector.Notification) error
}

// reconcileMandatesAndSubscriptions compares every non-final mandate and
// provider-managed subscription of the account with the provider's state
// (FetchMandate, FetchSubscription). A status mismatch is applied through the
// subscriptions service and recorded as fixed; if that fails the item stays
// open and ops are alerted. A provider that does not know the object, or is
// unreachable, is logged and skipped.
func (s *Service) reconcileMandatesAndSubscriptions(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, conn connector.Connector) error {
	if s.o.Subscriptions == nil {
		return nil
	}
	if mm, ok := conn.(connector.MandateManager); ok {
		var open []store.Mandate
		if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { open, err = store.ListOpenMandates(ctx, q, pa.ID); return }); err != nil {
			return err
		}
		for _, m := range open {
			run.Checked++
			snap, err := mm.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: m.ProviderMandateID})
			if err != nil {
				s.skipFetch(pa, "mandate", m.ProviderMandateID, err)
				continue
			}
			if snap.Status == "" || snap.Status == m.Status {
				continue
			}
			if snap.ProviderMandateID == "" {
				snap.ProviderMandateID = m.ProviderMandateID
			}
			it := recon.Item{Kind: recon.KindStatusMismatch, ProviderID: m.ProviderMandateID, AutoFix: true,
				Ours:   &recon.Record{ProviderID: m.ProviderMandateID, Status: m.Status, Amount: m.MaxAmount, Currency: m.Currency},
				Theirs: recon.Record{ProviderID: m.ProviderMandateID, Status: snap.Status, Amount: snap.MaxAmount, Currency: snap.Currency}}
			err = s.o.Subscriptions.HandleNotification(ctx, pa.ID, connector.Notification{Kind: connector.NotifyMandate,
				EventID: "recon:" + m.ProviderMandateID, EventType: "reconciliation.mandate", Mandate: &snap})
			if err := s.recordApplied(ctx, pa, run, it, "mandate", err); err != nil {
				return err
			}
		}
	}
	if sm, ok := conn.(connector.SubscriptionManager); ok {
		var open []store.Subscription
		if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
			open, err = store.ListOpenProviderSubscriptions(ctx, q, pa.ID)
			return
		}); err != nil {
			return err
		}
		for _, sub := range open {
			run.Checked++
			snap, err := sm.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sub.ProviderSubscriptionID})
			if err != nil {
				s.skipFetch(pa, "subscription", sub.ProviderSubscriptionID, err)
				continue
			}
			if !subscriptionDiffers(sub.Status, snap.Status) {
				continue
			}
			if snap.ProviderSubscriptionID == "" {
				snap.ProviderSubscriptionID = sub.ProviderSubscriptionID
			}
			it := recon.Item{Kind: recon.KindStatusMismatch, ProviderID: sub.ProviderSubscriptionID, AutoFix: true,
				Ours:   &recon.Record{ProviderID: sub.ProviderSubscriptionID, Status: sub.Status, Amount: sub.Amount, Currency: sub.Currency},
				Theirs: recon.Record{ProviderID: sub.ProviderSubscriptionID, Status: snap.Status, Amount: snap.Amount, Currency: snap.Currency}}
			err = s.o.Subscriptions.HandleNotification(ctx, pa.ID, connector.Notification{Kind: connector.NotifySubscription,
				EventID: "recon:" + sub.ProviderSubscriptionID, EventType: "reconciliation.subscription", Subscription: &snap})
			if err := s.recordApplied(ctx, pa, run, it, "subscription", err); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) skipFetch(pa store.ProviderAccount, what, id string, err error) {
	if errors.Is(err, connector.ErrNotFound) {
		s.o.Logger.Warn("reconcile: the provider does not know our "+what, "account", pa.ID, "id", id)
		return
	}
	s.o.Logger.Warn("reconcile: could not fetch "+what, "account", pa.ID, "id", id, "err", err)
}

func (s *Service) recordApplied(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, it recon.Item, what string, applyErr error) error {
	if applyErr != nil {
		s.o.Logger.Warn("reconcile: applying the provider's state failed", "account", pa.ID, what, it.ProviderID, "err", applyErr)
		it.AutoFix = false
		return s.record(ctx, pa, run, it, false, failedFix(what, it.ProviderID, applyErr))
	}
	return s.record(ctx, pa, run, it, true, recordNote{})
}

// subscriptionDiffers reports whether the provider's status is one our
// subscription should follow: the provider ended or canceled it, reports it
// past due, or activated one we still hold as incomplete. Finer differences
// (a trial our side calls trialing while the provider says active) are not
// mismatches.
func subscriptionDiffers(ours, theirs string) bool {
	cur := state.SubscriptionStatus(ours)
	switch theirs {
	case "canceled", "ended", "incomplete_expired":
		return cur != state.SubCanceled && cur != state.SubEnded && cur != state.SubIncompleteExpired
	case "active":
		return cur == state.SubIncomplete
	case "past_due":
		return cur == state.SubActive || cur == state.SubTrialing
	}
	return false
}
