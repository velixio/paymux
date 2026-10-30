package checkout

import (
	"context"
	"errors"
	"fmt"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// ImportRefund books a refund the provider made on its own (from its
// dashboard, say) as a refund of the payment it belongs to. snap must be what
// the provider's authenticated API returned for the refund (never a webhook
// body): its amount, currency, status and payment id are the truth.
//
// The refund is matched to one of the delivering account's own attempts by the
// provider payment id; anything else is not ours and imported is false with a
// nil error. A refund that is already known, ours or imported earlier, is only
// advanced, so delivering it again (or reconciling it) is harmless. A refund
// that cannot be booked (a currency that differs from the payment's, an
// attempt that was never captured) comes back as core.ErrInvalid or
// core.ErrNotRefundable for the caller to record.
func ImportRefund(ctx context.Context, db *store.DB, c *core.Service, accountID string, snap connector.RefundSnapshot) (imported bool, err error) {
	if snap.ProviderRefundID == "" || snap.ProviderPaymentID == "" {
		return false, nil
	}
	// It may be a refund we made, whose id we have not stored yet.
	var known store.Refund
	err = db.Read(ctx, func(q store.Querier) (err error) {
		known, err = store.FindRefund(ctx, q, accountID, snap.ProviderRef, snap.ProviderRefundID)
		return
	})
	switch {
	case err == nil:
		// The same check as when a refund is polled: what the provider returned has
		// to be this refund (its payment, receipt, amount and currency), or it is
		// not applied.
		var ka store.Attempt
		if err := db.Read(ctx, func(q store.Querier) (err error) { ka, err = store.GetAttempt(ctx, q, known.AttemptID); return }); err != nil {
			return false, err
		}
		if err := refundIsOurs(known, ka, snap); err != nil {
			return false, err
		}
		_, err = c.ApplyRefundSnapshot(ctx, known.ID, toRefundSnapshot(snap))
		return err == nil, err
	case !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	var a store.Attempt
	err = db.Read(ctx, func(q store.Querier) (err error) {
		a, err = store.FindAttempt(ctx, q, accountID, "", "", snap.ProviderPaymentID)
		return
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil // not one of our payments
	}
	if err != nil {
		return false, err
	}
	rs := toRefundSnapshot(snap)
	_, err = c.ImportProviderRefund(ctx, a.ID, core.ProviderRefund{ProviderRefundID: snap.ProviderRefundID, Status: rs.Status,
		Amount: snap.Amount, Currency: snap.Currency, FailureReason: rs.FailureReason})
	return err == nil, err
}

// importProviderRefund handles a refund notification for a refund Paymux has
// no row for: it fetches the refund from the provider and books it, or records
// the notification as unknown when it is not ours or cannot be booked.
func (s *Service) importProviderRefund(ctx context.Context, ev store.ProviderEventRow, n connector.Notification) error {
	rs := n.Refund
	unknown := func() error {
		s.o.Logger.Info("checkout: a refund notification is not one of our refunds or payments; recorded as unknown",
			"account", ev.ProviderAccountID, "refund", firstNonEmpty(rs.ProviderRefundID, rs.ProviderRef))
		return s.recordUnknown(ctx, ev, "refund", firstNonEmpty(rs.ProviderRefundID, rs.ProviderRef), n)
	}
	live, err := s.o.Conns.Get(ctx, ev.ProviderAccountID)
	if err != nil {
		return err
	}
	rf, ok := live.Connector.(connector.Refunder)
	if !ok {
		return unknown()
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snap, err := rf.FetchRefund(fctx, connector.RefundRef{ProviderRef: rs.ProviderRef, ProviderRefundID: rs.ProviderRefundID,
		ProviderPaymentID: rs.ProviderPaymentID})
	if errors.Is(err, connector.ErrNotFound) {
		return unknown()
	}
	if err != nil {
		return err // the job retries
	}
	imported, err := ImportRefund(ctx, s.o.DB, s.o.Core, ev.ProviderAccountID, snap)
	switch {
	case errors.Is(err, core.ErrInvalid), errors.Is(err, core.ErrNotRefundable), errors.Is(err, ErrNotOurs):
		s.o.Logger.Warn("checkout: a provider refund cannot be booked", "account", ev.ProviderAccountID, "refund", snap.ProviderRefundID, "err", err)
		return unknown()
	case err != nil:
		return fmt.Errorf("importing provider refund %s: %w", snap.ProviderRefundID, err)
	case !imported:
		return unknown()
	}
	return nil
}
