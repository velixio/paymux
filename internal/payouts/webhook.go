package payouts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// ErrUnknownPayout means no payout of this provider account matches the
// notification.
var ErrUnknownPayout = errors.New("payouts: no payout of this provider account matches the notification")

// ErrNotOurs means the provider returned a payout that does not match ours.
var ErrNotOurs = errors.New("payouts: the provider's payout does not match ours")

// SyncNotification handles a payout webhook the way every webhook is handled:
// as a hint. The payout it points at must be one of this provider account's
// (found by our reference or the provider's payout id); the payout is then
// fetched from the provider by what we hold, checked against our row, and that
// is what is applied. The webhook's own status, amount and ids are never used.
func (s *Service) SyncNotification(ctx context.Context, providerAccountID string, n connector.Notification) error {
	if n.Payout == nil {
		return fmt.Errorf("payouts: %q is not a payout notification", n.Kind)
	}
	var p store.Payout
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		p, err = store.FindPayout(ctx, q, providerAccountID, n.Payout.ProviderRef, n.Payout.ProviderPayoutID)
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnknownPayout
	}
	if err != nil {
		return err
	}
	po, err := s.payouter(ctx, p.ProviderAccountID)
	if err != nil {
		return err
	}
	snap, err := po.FetchPayout(ctx, connector.PayoutRef{ProviderRef: p.ProviderRef, ProviderPayoutID: p.ProviderPayoutID})
	if err != nil {
		return err
	}
	if err := payoutIsOurs(p, snap); err != nil {
		s.o.Logger.Warn("payouts: the provider returned a payout that is not this payout's; ignored", "payout", p.ID,
			"account", p.ProviderAccountID, "err", err)
		return err
	}
	return s.apply(ctx, p.ID, snap)
}

// payoutIsOurs checks, where the provider says, that its payout has our
// reference, amount and currency.
func payoutIsOurs(p store.Payout, snap connector.PayoutSnapshot) error {
	switch {
	case snap.ProviderRef != "" && snap.ProviderRef != p.ProviderRef:
		return fmt.Errorf("%w: its reference is %q, ours is %q", ErrNotOurs, snap.ProviderRef, p.ProviderRef)
	case snap.ProviderPayoutID != "" && p.ProviderPayoutID != "" && snap.ProviderPayoutID != p.ProviderPayoutID:
		return fmt.Errorf("%w: its payout id is %q, ours is %q", ErrNotOurs, snap.ProviderPayoutID, p.ProviderPayoutID)
	case snap.Amount > 0 && snap.Amount != p.Amount:
		return fmt.Errorf("%w: it paid %d, we sent %d", ErrNotOurs, snap.Amount, p.Amount)
	case snap.Currency != "" && !strings.EqualFold(snap.Currency, p.Currency):
		return fmt.Errorf("%w: it paid in %s, we sent %s", ErrNotOurs, snap.Currency, p.Currency)
	}
	return nil
}
