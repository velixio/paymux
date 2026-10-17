package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// JobPoll is the scheduled poller job.
const JobPoll = "poll.due"

// PollWindow is how long a non-terminal attempt or refund is polled.
const PollWindow = 72 * time.Hour

var pollLadder = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

// PollDelay is the wait before poll number count+1 (spec section 6: 1m, 2m,
// 5m, 15m, 1h, 6h, then every 24h).
func PollDelay(count int) time.Duration {
	return pollLadder[min(max(count, 0), len(pollLadder)-1)]
}

// Register installs the job handlers (refund.submit, provider event
// processing, polling) on q and schedules the poller every minute.
func (s *Service) Register(q *jobs.Queue) error {
	q.Register(core.JobRefundSubmit, s.SubmitRefund)
	q.Register(inbound.JobProcess, s.ProcessProviderEvent)
	q.Register(inbound.JobFetchOnly, s.ProcessProviderEvent)
	q.Register(JobPoll, func(ctx context.Context, _ json.RawMessage) error { return s.PollDue(ctx) })
	return q.AddSchedule(jobs.Schedule{Name: JobPoll, Kind: JobPoll, Every: time.Minute})
}

// ---- inbound provider events ----

// ProcessProviderEvent handles provider_event.process and .fetch_only jobs.
// A webhook is only a hint: for every notification it looks the object up
// among the delivering account's own attempts and refunds, fetches the
// provider's truth through the connector and applies that. Verified and
// unverified events take the same path, so a forged event can at most cause
// a fetch. Objects Paymux does not know are recorded, not guessed.
func (s *Service) ProcessProviderEvent(ctx context.Context, payload json.RawMessage) error {
	var jp inbound.JobPayload
	if err := json.Unmarshal(payload, &jp); err != nil {
		return jobs.Permanent(fmt.Errorf("checkout: bad job payload: %w", err))
	}
	var ev store.ProviderEventRow
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		ev, err = store.GetProviderEventByID(ctx, q, jp.ProviderEventID)
		return
	})
	if err != nil {
		return jobs.Permanent(fmt.Errorf("checkout: provider event %s: %w", jp.ProviderEventID, err))
	}
	if ev.ProviderAccountID != jp.ProviderAccountID {
		return jobs.Permanent(errors.New("checkout: provider event belongs to another account"))
	}
	live, err := s.o.Conns.Get(ctx, ev.ProviderAccountID)
	if err != nil {
		return jobs.Permanent(err)
	}
	var headers map[string][]string
	_ = json.Unmarshal(ev.Headers, &headers)
	notes, err := live.Connector.ParseWebhook(ctx, connector.WebhookRequest{Method: "POST", Headers: headers, Body: ev.Body,
		ReceivedAt: ev.ReceivedAt.UnixMilli()})
	if err != nil {
		s.markProcessed(ctx, ev.ID, "parse: "+err.Error())
		return jobs.Permanent(err)
	}
	var firstErr error
	for _, n := range notes {
		if err := s.handleNotification(ctx, ev, n); err != nil {
			s.o.Logger.Error("checkout: notification failed", "event", ev.ID, "kind", n.Kind, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return firstErr // the job retries; applying is idempotent
	}
	s.markProcessed(ctx, ev.ID, "")
	return nil
}

func (s *Service) markProcessed(ctx context.Context, id, errText string) {
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		return store.MarkProviderEventProcessed(ctx, tx, id, s.o.Now().UTC(), errText)
	})
	if err != nil {
		s.o.Logger.Error("checkout: marking event processed", "event", id, "err", err)
	}
}

func (s *Service) handleNotification(ctx context.Context, ev store.ProviderEventRow, n connector.Notification) error {
	account := ev.ProviderAccountID
	switch {
	case n.Kind == connector.NotifyPayment && n.Payment != nil:
		ps := n.Payment
		var a store.Attempt
		err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
			a, err = store.FindAttempt(ctx, q, account, ps.ProviderRef, ps.ProviderOrderID, ps.ProviderPaymentID)
			return
		})
		if errors.Is(err, store.ErrNotFound) {
			return s.recordUnknown(ctx, ev, "payment", firstNonEmpty(ps.ProviderPaymentID, firstNonEmpty(ps.ProviderOrderID, ps.ProviderRef)), n)
		}
		if err != nil {
			return err
		}
		_, err = s.syncAttempt(ctx, a.ID, connector.PaymentRef{ProviderOrderID: ps.ProviderOrderID, ProviderPaymentID: ps.ProviderPaymentID})
		return err
	case n.Kind == connector.NotifyRefund && n.Refund != nil:
		rs := n.Refund
		var r store.Refund
		err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
			r, err = store.FindRefund(ctx, q, account, rs.ProviderRef, rs.ProviderRefundID)
			return
		})
		if errors.Is(err, store.ErrNotFound) {
			// TODO(spec): provider-initiated refunds (initiated_by=provider) are
			// created by the disputes/refunds lane; until then they are recorded.
			return s.recordUnknown(ctx, ev, "refund", firstNonEmpty(rs.ProviderRefundID, rs.ProviderRef), n)
		}
		if err != nil {
			return err
		}
		_, err = s.syncRefund(ctx, r.ID, rs.ProviderRefundID)
		return err
	case n.Kind == connector.NotifyDispute && n.Dispute != nil:
		// TODO(spec): the dispute lane looks disputes up by provider_dispute_id
		// within this account and applies FetchDispute; until its tables exist
		// every dispute is recorded as unknown.
		return s.recordUnknown(ctx, ev, "dispute", n.Dispute.ProviderDisputeID, n)
	}
	// TODO(spec): mandate and subscription notifications belong to the
	// subscriptions lane.
	id := n.EventID
	return s.recordUnknown(ctx, ev, string(n.Kind), id, n)
}

func (s *Service) recordUnknown(ctx context.Context, ev store.ProviderEventRow, kind, providerID string, n connector.Notification) error {
	detail, _ := json.Marshal(n)
	return s.o.DB.Write(ctx, func(tx store.Tx) error {
		return store.InsertUnknownObject(ctx, tx, store.UnknownObject{ID: ids.New("uo"), ProviderAccountID: ev.ProviderAccountID,
			ProviderEventID: ev.ProviderEventID, Kind: kind, ProviderID: providerID, Detail: string(detail), CreatedAt: s.o.Now().UTC()})
	})
}

// ---- refunds ----

func toRefundSnapshot(s connector.RefundSnapshot) core.RefundSnapshot {
	out := core.RefundSnapshot{ProviderRefundID: s.ProviderRefundID}
	switch s.Status {
	case connector.RefundSucceeded:
		out.Status = state.RefundSucceeded
	case connector.RefundFailed:
		out.Status = state.RefundFailed
		out.FailureReason = s.RawStatus
	default:
		out.Status = state.RefundPending
	}
	return out
}

type refundJob struct {
	RefundID string `json:"refund_id"`
}

// SubmitRefund handles the refund.submit job: it sends the pending refund to
// the provider with provider_ref as the idempotency token (so retries are
// safe) and applies the answer. A provider refusal (ErrNotRefundable) fails
// the refund; any other error is returned so the job retries.
func (s *Service) SubmitRefund(ctx context.Context, payload json.RawMessage) error {
	var jp refundJob
	if err := json.Unmarshal(payload, &jp); err != nil || jp.RefundID == "" {
		return jobs.Permanent(errors.New("checkout: bad refund.submit payload"))
	}
	r, err := s.o.Core.GetRefundByID(ctx, jp.RefundID)
	if err != nil {
		return jobs.Permanent(err)
	}
	if r.Status != string(state.RefundPending) {
		return nil // finished (or failed) already
	}
	a, err := s.o.Core.GetAttempt(ctx, r.AttemptID)
	if err != nil {
		return jobs.Permanent(err)
	}
	live, err := s.o.Conns.Get(ctx, a.ProviderAccountID)
	if err != nil {
		return err
	}
	rf, ok := live.Connector.(connector.Refunder)
	if !ok {
		return s.failRefund(ctx, r, "the provider connector does not support refunds")
	}
	if a.ProviderPaymentID == "" {
		return errors.New("checkout: attempt has no provider payment id yet; retrying")
	}
	rctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snap, err := rf.Refund(rctx, connector.RefundRequest{ProviderRef: r.ProviderRef, ProviderPaymentID: a.ProviderPaymentID,
		Amount: r.Amount, Currency: r.Currency, Reason: r.Reason})
	var nr connector.ErrNotRefundable
	if errors.As(err, &nr) {
		return s.failRefund(ctx, r, nr.Reason)
	}
	if err != nil {
		return err
	}
	_, err = s.o.Core.ApplyRefundSnapshot(ctx, r.ID, toRefundSnapshot(snap))
	return err
}

func (s *Service) failRefund(ctx context.Context, r store.Refund, reason string) error {
	_, err := s.o.Core.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundFailed, FailureReason: reason})
	return err
}

// SyncRefund fetches the provider's state of a refund and applies it.
func (s *Service) SyncRefund(ctx context.Context, refundID string) (core.RefundApplyResult, error) {
	return s.syncRefund(ctx, refundID, "")
}

func (s *Service) syncRefund(ctx context.Context, refundID, providerRefundID string) (core.RefundApplyResult, error) {
	r, err := s.o.Core.GetRefundByID(ctx, refundID)
	if err != nil {
		return core.RefundApplyResult{}, err
	}
	a, err := s.o.Core.GetAttempt(ctx, r.AttemptID)
	if err != nil {
		return core.RefundApplyResult{}, err
	}
	live, err := s.o.Conns.Get(ctx, a.ProviderAccountID)
	if err != nil {
		return core.RefundApplyResult{}, err
	}
	rf, ok := live.Connector.(connector.Refunder)
	if !ok {
		return core.RefundApplyResult{Refund: r}, nil
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	snap, err := rf.FetchRefund(fctx, connector.RefundRef{ProviderRef: r.ProviderRef,
		ProviderRefundID: firstNonEmpty(r.ProviderRefundID, providerRefundID), ProviderPaymentID: a.ProviderPaymentID})
	if errors.Is(err, connector.ErrNotFound) {
		return core.RefundApplyResult{Refund: r}, nil // not submitted yet
	}
	if err != nil {
		return core.RefundApplyResult{}, err
	}
	return s.o.Core.ApplyRefundSnapshot(ctx, r.ID, toRefundSnapshot(snap))
}

// ---- poller ----

const pollBatch = 200

// PollDue polls non-terminal attempts and refunds created within PollWindow
// whose next poll is due, and schedules each one's next poll on the ladder.
// Provider errors are logged and do not fail the job; the ladder retries.
func (s *Service) PollDue(ctx context.Context) error {
	now := s.o.Now().UTC()
	cutoff := now.Add(-PollWindow)
	var attempts, refunds []store.PollItem
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		if attempts, err = store.ListDueAttempts(ctx, q, now, cutoff, pollBatch); err != nil {
			return err
		}
		refunds, err = store.ListDueRefunds(ctx, q, now, cutoff, pollBatch)
		return
	})
	if err != nil {
		return err
	}
	for _, it := range attempts {
		if _, err := s.SyncAttempt(ctx, it.ID); err != nil {
			s.o.Logger.Warn("poll: attempt", "attempt", it.ID, "err", err)
		}
		next := now.Add(PollDelay(it.PollCount + 1))
		if err := s.o.DB.Write(ctx, func(tx store.Tx) error { return store.BumpAttemptPoll(ctx, tx, it.ID, it.PollCount+1, next) }); err != nil {
			return err
		}
	}
	for _, it := range refunds {
		if _, err := s.SyncRefund(ctx, it.ID); err != nil {
			s.o.Logger.Warn("poll: refund", "refund", it.ID, "err", err)
		}
		next := now.Add(PollDelay(it.PollCount + 1))
		if err := s.o.DB.Write(ctx, func(tx store.Tx) error { return store.BumpRefundPoll(ctx, tx, it.ID, it.PollCount+1, next) }); err != nil {
			return err
		}
	}
	return nil
}
