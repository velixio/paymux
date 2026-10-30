package core

import (
	"context"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
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
		if skip := s.askProviderFirst(ctx, id); skip {
			continue
		}
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

// askProviderFirst fetches the provider's truth for each open attempt of the
// payment before it is expired, so that a payment the customer has paid (and
// never came back from) is applied rather than expired. It reports whether the
// payment is to be left alone this time: the provider could not be asked, or
// answered with a success (or anything else) that moved the payment on. "Not
// found" is an answer: nothing was paid, and the payment may expire.
func (s *Service) askProviderFirst(ctx context.Context, paymentID string) (skip bool) {
	if s.syncer == nil {
		return false
	}
	var attempts []store.Attempt
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { attempts, err = store.ListAttempts(ctx, q, paymentID); return }); err != nil {
		return true
	}
	cutoff := s.nowUTC().UnixMilli() - openAttemptWindow
	for _, a := range attempts {
		switch state.AttemptStatus(a.Status) {
		case state.AttemptCreated, state.AttemptPending, state.AttemptAuthorized:
		default:
			continue
		}
		if a.CreatedAt.UnixMilli() <= cutoff || (a.NextAction == "" && a.ProviderOrderID == "" && a.ProviderPaymentID == "") {
			continue
		}
		if _, err := s.syncer.SyncAttempt(ctx, a.ID); err != nil {
			return true // the provider could not be asked: not now
		}
	}
	var p store.Payment
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { p, err = store.GetPayment(ctx, q, paymentID); return }); err != nil {
		return true
	}
	return p.Status != string(state.PaymentCreated) && p.Status != string(state.PaymentRequiresAction)
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
