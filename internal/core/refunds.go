package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
)

// JobRefundSubmit is the job kind that sends a pending refund to its provider.
const JobRefundSubmit = "refund.submit"

// RegisterJobs registers the handlers of this package on q.
func RegisterJobs(q *jobs.Queue) {
	// TODO(connector lane): look up the refund and its attempt's provider
	// account, call connector.Refund with provider_ref as the idempotency
	// token, and move the refund to succeeded or failed via state.RefundNext.
	q.Register(JobRefundSubmit, func(context.Context, json.RawMessage) error { return nil })
}

// CreateRefundInput is the input of CreateRefund.
type CreateRefundInput struct {
	Scope Scope
	Actor audit.Actor
	// InitiatedBy is api, dashboard, provider or system.
	InitiatedBy string
	PaymentID   string
	// AttemptID is the attempt to refund; default: the payment's succeeded
	// (captured, non-duplicate) attempt.
	AttemptID string
	// Amount is in minor units; 0 means everything still refundable.
	Amount int64
	Reason string
	// IdempotencyKey is the request's Idempotency-Key. The refund is stored
	// with it: a repeat returns the refund the key made first.
	IdempotencyKey string
}

// CreateRefund checks the refundable balance of an attempt under a lock on
// the payment (SQLite's single writer, Postgres SELECT ... FOR UPDATE),
// inserts a pending refund with a fresh provider_ref, emits refund.created and
// enqueues refund.submit, all in one transaction.
func (s *Service) CreateRefund(ctx context.Context, in CreateRefundInput) (store.Refund, error) {
	if err := in.Scope.validate(); err != nil {
		return store.Refund{}, err
	}
	if !slices.Contains([]string{"api", "dashboard", "provider", "system"}, in.InitiatedBy) {
		return store.Refund{}, newErr(ErrInvalid, "unknown initiator %q", in.InitiatedBy)
	}
	if in.Amount < 0 || in.Amount > maxAmount {
		return store.Refund{}, newErr(ErrInvalid, "amount must be positive")
	}
	if len(in.Reason) > 500 {
		return store.Refund{}, newErr(ErrInvalid, "reason too long (max 500)")
	}
	var r store.Refund
	err := s.db.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPaymentForUpdate(ctx, tx, in.PaymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		if p.AppID != in.Scope.AppID || p.Mode != in.Scope.Mode {
			return newErr(ErrNotFound, "payment not found")
		}
		if in.IdempotencyKey != "" {
			// Under the payment lock, so two requests with one key cannot both pass.
			prev, err := store.FindRefundByKey(ctx, tx, in.Scope.AppID, in.Scope.Mode, in.IdempotencyKey)
			switch {
			case err == nil:
				if prev.PaymentID != p.ID || in.Amount != 0 && prev.Amount != in.Amount ||
					in.AttemptID != "" && prev.AttemptID != in.AttemptID {
					return newErr(ErrConflict, "this Idempotency-Key already made a different refund")
				}
				r = prev
				return nil
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		// The card network has taken (or is holding) this money back; a refund of
		// ours as well would pay the customer twice. Refunds the provider made
		// itself are booked through ImportProviderRefund, not here.
		if in.InitiatedBy != "provider" && (p.HasFlag("disputed") || p.HasFlag("dispute_lost")) {
			return newErr(ErrNotRefundable, "the payment is in dispute or its dispute was lost; the money is already being taken back")
		}
		attempts, err := store.ListAttempts(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		var target *store.Attempt
		for i := range attempts {
			a := &attempts[i]
			switch {
			case in.AttemptID != "" && a.ID == in.AttemptID:
				target = a
			case in.AttemptID == "" && a.Status == string(state.AttemptCaptured) && !a.Duplicate && target == nil:
				target = a
			}
		}
		if target == nil {
			if in.AttemptID != "" {
				return newErr(ErrInvalid, "attempt does not belong to this payment")
			}
			return newErr(ErrNotRefundable, "the payment has no succeeded attempt to refund")
		}
		if target.Status != string(state.AttemptCaptured) {
			return newErr(ErrNotRefundable, "attempt is %s, only captured attempts can be refunded", target.Status)
		}
		active, err := store.SumActiveRefunds(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		remaining := target.AmountReceived - active
		amount := in.Amount
		if amount == 0 {
			amount = remaining
		}
		if amount <= 0 || amount > remaining {
			return newErr(ErrRefundExceeds, "refund of %d exceeds the refundable balance of %d", in.Amount, max(remaining, 0))
		}
		r, err = s.insertRefund(ctx, tx, p, *target, amount, in.Reason, in.InitiatedBy, in.IdempotencyKey, in.Actor)
		return err
	})
	return r, err
}

// insertRefund writes a pending refund for attempt a, then emits refund.created,
// enqueues refund.submit and audits it. The caller holds the payment lock and
// has validated the amount.
func (s *Service) insertRefund(ctx context.Context, tx store.Tx, p store.Payment, a store.Attempt, amount int64,
	reason, initiatedBy, idempotencyKey string, actor audit.Actor) (store.Refund, error) {
	now := s.nowUTC()
	r := store.Refund{ID: ids.New("ref"), OrgID: p.OrgID, AppID: p.AppID, Mode: p.Mode, PaymentID: p.ID, AttemptID: a.ID,
		Amount: amount, Currency: a.CurrencyReceived, Status: string(state.RefundPending), Reason: reason,
		ProviderRef: ids.ProviderRef(), InitiatedBy: initiatedBy, Actor: actor.ID, CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey}
	if err := store.InsertRefund(ctx, tx, r); err != nil {
		return store.Refund{}, err
	}
	if err := s.emitter.Emit(ctx, tx, events.Event{OrgID: r.OrgID, AppID: r.AppID, Mode: r.Mode, Type: "refund.created",
		ObjectType: "refund", ObjectID: r.ID, ObjectVersion: 1, Data: ViewRefund(r)}); err != nil {
		return store.Refund{}, err
	}
	if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobRefundSubmit, Payload: map[string]string{"refund_id": r.ID},
		DedupeKey: JobRefundSubmit + ":" + r.ID}); err != nil {
		return store.Refund{}, err
	}
	err := audit.Record(ctx, tx, audit.Entry{OrgID: r.OrgID, Actor: actor, Action: "refund.create", Target: r.ID,
		After: ViewRefund(r)})
	return r, err
}

// GetRefundByID returns a refund without tenant scoping, for jobs and
// provider-event processing that arrive with an internal id.
func (s *Service) GetRefundByID(ctx context.Context, id string) (store.Refund, error) {
	var r store.Refund
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		r, err = store.GetRefund(ctx, q, id)
		return
	})
	return r, mapStoreErr(err, "refund")
}

// GetRefund returns a refund of the scope's app and mode.
func (s *Service) GetRefund(ctx context.Context, sc Scope, id string) (store.Refund, error) {
	if err := sc.validate(); err != nil {
		return store.Refund{}, err
	}
	var r store.Refund
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		r, err = store.GetRefund(ctx, q, id)
		return
	})
	if err != nil {
		return store.Refund{}, mapStoreErr(err, "refund")
	}
	if r.AppID != sc.AppID || r.Mode != sc.Mode {
		return store.Refund{}, newErr(ErrNotFound, "refund not found")
	}
	return r, nil
}
