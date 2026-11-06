package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

var threeDSValues = []string{connector.ThreeDSPassed, connector.ThreeDSFailed, connector.ThreeDSNotVerified}

// recordTry keeps the try behind a snapshot before the snapshot is applied, so
// a snapshot the attempt ignores (a late failure) still reaches the history.
// History is observational: failing to write it never stops a payment.
func (s *Service) recordTry(ctx context.Context, attemptID string, snap Snapshot) {
	if snap.ProviderPaymentID == "" {
		return
	}
	a, err := s.GetAttempt(ctx, attemptID)
	if err != nil {
		slog.Warn("core: the attempt of a provider try could not be read", "attempt", attemptID, "err", err)
		return
	}
	s.RecordTry(ctx, a, snap)
}

// RecordTry stores one provider try of a. The caller has checked the snapshot
// belongs to a. A try with a failure is failed even while the provider keeps
// the payment open for another go.
func (s *Service) RecordTry(ctx context.Context, a store.Attempt, snap Snapshot) {
	if snap.ProviderPaymentID == "" {
		return
	}
	status := snap.RawStatus
	if status == "" {
		status = string(snap.Status)
	}
	switch {
	case snap.Status == state.AttemptCaptured:
		status = string(state.AttemptCaptured)
	case snap.Status == state.AttemptFailed || snap.FailureCode != "" || snap.FailureMessage != "":
		status = string(state.AttemptFailed)
	case snap.Status == state.AttemptAuthorized:
		status = string(state.AttemptAuthorized)
	}
	now := s.nowUTC()
	created := snap.ProviderCreatedAt
	if created.IsZero() {
		created = now
	}
	threeDS := snap.ThreeDS
	if !slices.Contains(threeDSValues, threeDS) {
		threeDS = ""
	}
	try := store.ProviderPaymentAttempt{ProviderAccountID: a.ProviderAccountID, ProviderPaymentID: snap.ProviderPaymentID, TryID: snap.TryID,
		AttemptID: a.ID, ProviderOrderID: firstNonEmpty(snap.ProviderOrderID, a.ProviderOrderID), Status: status, Method: snap.Method,
		FailureCode: snap.FailureCode, FailureMessage: snap.FailureMessage, Amount: snap.Amount, Currency: strings.ToUpper(snap.Currency),
		MethodDetails: tryJSON(safeMethodDetails(snap.MethodDetails), snapMethodEmpty), RiskDetails: tryJSON(safeRisk(snap.Risk), snapRiskEmpty),
		ThreeDS: threeDS, CreatedAt: created, UpdatedAt: now}
	if err := s.db.Write(ctx, func(tx store.Tx) error { return store.UpsertProviderPaymentAttempt(ctx, tx, try) }); err != nil {
		slog.Warn("core: a provider try could not be recorded", "attempt", a.ID, "provider_payment", snap.ProviderPaymentID, "err", err)
	}
}

func snapMethodEmpty(d connector.PaymentMethodDetails) bool { return d.Empty() }

// A risk with only an unverified 3-D Secure flag still counts as nothing to show.
func snapRiskEmpty(r connector.PaymentRisk) bool { return r.Empty() }

func tryJSON[T any](v T, empty func(T) bool) string {
	if empty(v) {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}
