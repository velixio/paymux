package core

import (
	"context"
	"time"

	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

// RenewalInput describes the payment of one subscription renewal.
type RenewalInput struct {
	OrgID, AppID, Mode, SubscriptionID, CustomerID string
	Description, ReturnURL                         string
	Metadata                                       map[string]string
	Amount                                         int64
	Currency                                       string
	// ProviderAccountID is the account the renewal is charged on.
	ProviderAccountID string
}

// CreateRenewalPayment creates, inside tx, a `renewal` Payment of a
// subscription and its first Attempt (status created) on the provider account,
// and emits payment.created. Everything after that (charging, snapshots,
// invoices, refunds) is the ordinary payment machinery. The renewal has no pay
// page, so its client secret is generated and discarded.
func (s *Service) CreateRenewalPayment(ctx context.Context, tx store.Tx, in RenewalInput) (store.Payment, store.Attempt, error) {
	if in.Amount <= 0 || in.Amount > maxAmount || in.SubscriptionID == "" {
		return store.Payment{}, store.Attempt{}, newErr(ErrInvalid, "a renewal needs a subscription and a positive amount")
	}
	if _, err := money.Exponent(in.Currency); err != nil {
		return store.Payment{}, store.Attempt{}, newErr(ErrInvalid, "unknown currency %q", in.Currency)
	}
	pa, err := store.GetProviderAccount(ctx, tx, in.ProviderAccountID)
	if err != nil {
		return store.Payment{}, store.Attempt{}, mapStoreErr(err, "provider account")
	}
	if pa.Mode != in.Mode || pa.OrgID != in.OrgID {
		return store.Payment{}, store.Attempt{}, newErr(ErrInvalid, "provider account is in another mode")
	}
	ttl, err := settings.ResolveWith(ctx, tx, in.AppID, "payment_ttl")
	if err != nil {
		return store.Payment{}, store.Attempt{}, err
	}
	var minutes int64
	if err := ttl.Decode(&minutes); err != nil {
		return store.Payment{}, store.Attempt{}, err
	}
	now := s.nowUTC()
	p := store.Payment{ID: ids.New("pay"), OrgID: in.OrgID, AppID: in.AppID, Mode: in.Mode, Kind: "renewal",
		SubscriptionID: in.SubscriptionID, Amount: in.Amount, Currency: in.Currency, Status: string(state.PaymentCreated),
		Description: in.Description, CustomerID: in.CustomerID, Metadata: in.Metadata, ReturnURL: in.ReturnURL,
		ClientSecretHash: hashSecret(newClientSecret()), ExpiresAt: now.Add(time.Duration(minutes) * time.Minute),
		Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.InsertPayment(ctx, tx, p); err != nil {
		return store.Payment{}, store.Attempt{}, err
	}
	a := store.Attempt{ID: ids.New("att"), OrgID: in.OrgID, Mode: in.Mode, PaymentID: p.ID, ProviderAccountID: pa.ID,
		ProviderRef: ids.ProviderRef(), Status: string(state.AttemptCreated), CreatedAt: now, UpdatedAt: now}
	if err := store.InsertAttempt(ctx, tx, a); err != nil {
		return store.Payment{}, store.Attempt{}, err
	}
	if err := s.emitPayment(ctx, tx, "payment.created", p, "", ""); err != nil {
		return store.Payment{}, store.Attempt{}, err
	}
	return p, a, nil
}
