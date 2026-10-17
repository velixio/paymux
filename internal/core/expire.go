package core

import (
	"context"

	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/store"
)

// openAttemptWindow is how long an attempt that reached a provider counts as
// still open: the poller's window (spec section 6).
const openAttemptWindow = 72 * 60 * 60 * 1000 // milliseconds

// ExpireDue moves up to limit payments past expires_at to expired. A payment
// is skipped while it has an open attempt: pending or authorized, or created
// and already at the provider, within the 72h polling window. A late success
// on an expired payment is still accepted by ApplyAttemptSnapshot. It returns
// how many payments it expired.
func (s *Service) ExpireDue(ctx context.Context, limit int) (int, error) {
	now := s.nowUTC()
	var ids []string
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		ids, err = store.ListExpirablePayments(ctx, q, now, now.UnixMilli()-openAttemptWindow, limit)
		return
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		done, err := s.expireOne(ctx, id)
		if err != nil {
			return n, err
		}
		if done {
			n++
		}
	}
	return n, nil
}

func (s *Service) expireOne(ctx context.Context, id string) (bool, error) {
	var done bool
	err := s.db.Write(ctx, func(tx store.Tx) error {
		done = false
		p, err := store.GetPaymentForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		now := s.nowUTC()
		if !now.After(p.ExpiresAt) {
			return nil
		}
		out := state.PaymentNext(state.PaymentStatus(p.Status), state.EvExpired)
		if out.Action != state.Apply {
			return nil
		}
		p.Status = string(out.Next)
		if err := store.UpdatePayment(ctx, tx, &p, now); err != nil {
			return err
		}
		done = true
		return s.emitPayment(ctx, tx, "payment.expired", p, "", "")
	})
	return done, err
}
