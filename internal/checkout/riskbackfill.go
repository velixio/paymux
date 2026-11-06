package checkout

import (
	"context"
	"errors"
	"time"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// JobRiskBackfill is the scheduled job that collects risk reviews.
const JobRiskBackfill = "risk.backfill"

const (
	// RiskBatch is the most provider calls one run makes, so a large history
	// fills in gradually without pressing on the provider's rate limit.
	RiskBatch = 20
	// RiskSettleDelay is how long after an attempt settles without a score it is
	// asked again, giving the provider's fraud scoring time to finish.
	RiskSettleDelay = 10 * time.Minute
)

// fetchRiskOnReceipt reads the review as soon as a payment settles. A lookup by
// order can return a payment without its review, so ask for the payment itself.
// It is best effort: the payment is already applied, and an attempt left without
// a score is picked up by BackfillRisk.
func (s *Service) fetchRiskOnReceipt(ctx context.Context, live connectors.Live, res core.ApplyResult, ref connector.PaymentRef, snap connector.PaymentSnapshot) {
	id := res.Attempt.ProviderPaymentID
	settled := res.Attempt.Status == string(state.AttemptCaptured) || res.Attempt.Status == string(state.AttemptFailed)
	if !res.Changed || !settled || snap.Risk.Score != nil || ref.ProviderPaymentID != "" || id == "" || id != snap.ProviderPaymentID {
		return
	}
	if _, info, ok := connector.Lookup(live.Account.Connector); !ok || !info.ReportsPaymentRisk {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	full, err := live.Connector.FetchPayment(fctx, connector.PaymentRef{ProviderPaymentID: id})
	if err == nil && full.ProviderPaymentID != id {
		return
	}
	if err == nil {
		err = s.o.Core.SaveAttemptRisk(ctx, res.Attempt.ID, full.Risk)
	}
	if err != nil {
		s.o.Logger.Warn("checkout: risk review not read on receipt; retrying later", "attempt", res.Attempt.ID, "err", err)
	}
}

// BackfillRisk fetches the risk review of settled attempts not yet checked,
// old and new, from connectors that report one. It reads by provider payment
// id and saves the review only: what a provider says about the payment's status
// here never changes the payment. A provider that is down keeps its attempts
// for the next run.
func (s *Service) BackfillRisk(ctx context.Context) error {
	var accounts []store.ProviderAccount
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		accounts, err = store.ListActiveProviderAccounts(ctx, q)
		return err
	}); err != nil {
		return err
	}
	budget := RiskBatch
	cutoff := s.o.Now().UTC().Add(-RiskSettleDelay)
	for _, pa := range accounts {
		if budget == 0 {
			return nil
		}
		if _, info, ok := connector.Lookup(pa.Connector); !ok || !info.ReportsPaymentRisk {
			continue
		}
		used, err := s.backfillAccountRisk(ctx, pa, cutoff, budget)
		if err != nil {
			return err
		}
		budget -= used
	}
	return nil
}

func (s *Service) backfillAccountRisk(ctx context.Context, pa store.ProviderAccount, cutoff time.Time, limit int) (used int, err error) {
	var items []store.RiskCheckItem
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		items, err = store.ListAttemptsToRiskCheck(ctx, q, pa.ID, cutoff, limit)
		return err
	}); err != nil || len(items) == 0 {
		return 0, err
	}
	live, err := s.o.Conns.Get(ctx, pa.ID)
	if err != nil {
		s.o.Logger.Warn("risk backfill: connector unavailable", "account", pa.ID, "err", err)
		return 0, nil
	}
	for _, it := range items {
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		snap, ferr := live.Connector.FetchPayment(fctx, connector.PaymentRef{ProviderPaymentID: it.ProviderPaymentID})
		cancel()
		used++
		var review connector.PaymentRisk
		switch {
		case ferr == nil:
			if snap.ProviderPaymentID == it.ProviderPaymentID {
				review = snap.Risk
			} else {
				s.o.Logger.Warn("risk backfill: the provider returned another payment; ignored", "attempt", it.ID, "account", pa.ID)
			}
		case riskRetryLater(ferr):
			s.o.Logger.Warn("risk backfill: provider unavailable, retrying later", "account", pa.ID, "err", ferr)
			return used, nil
		case !errors.Is(ferr, connector.ErrNotFound):
			s.o.Logger.Warn("risk backfill: attempt skipped", "attempt", it.ID, "account", pa.ID, "err", ferr)
		}
		if err := s.o.Core.RecordAttemptRisk(ctx, it.ID, review); err != nil {
			return used, err
		}
	}
	return used, nil
}

// riskRetryLater reports a failure of the provider or the account, not of this
// attempt: stop for now and keep the attempt unchecked.
func riskRetryLater(err error) bool {
	var limited connector.ErrRateLimited
	return errors.As(err, &limited) || errors.Is(err, connector.ErrTemporary) || errors.Is(err, connector.ErrInvalidCredentials) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
