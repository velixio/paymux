// Package checkout is the integration layer between core, the connector
// runtime and routing: it routes payments, starts provider checkouts, syncs
// attempts and refunds from the provider's truth, and runs the inbound,
// refund-submit and polling jobs.
package checkout

import (
	"context"
	"errors"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/routing"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

// Routing evaluates the routing setting (app rules, org rules, catch-all)
// against the app's provider accounts in the payment's mode. It implements
// core.Router for create-time validation; the account is picked later, on
// the pay page, with the circuit breaker applied.
type Routing struct{ breaker *routing.Breaker }

var _ core.Router = (*Routing)(nil)

// NewRouting returns a Routing using b to deprioritise failing accounts.
func NewRouting(b *routing.Breaker) *Routing { return &Routing{breaker: b} }

type routingDoc struct {
	Rules    []routing.Rule `json:"rules"`
	CatchAll *routing.Rule  `json:"catch_all"`
}

func (r *Routing) config(ctx context.Context, q store.Queryer, app store.App) (routing.Config, error) {
	var cfg routing.Config
	org, err := settings.ResolveOrgWith(ctx, q, app.OrgID, "routing")
	if err != nil {
		return cfg, err
	}
	var orgDoc routingDoc
	if err := org.Decode(&orgDoc); err != nil {
		return cfg, err
	}
	cfg.Org = orgDoc.Rules
	if orgDoc.CatchAll != nil {
		cfg.CatchAll = *orgDoc.CatchAll
	}
	ov, err := settings.ResolveWith(ctx, q, app.ID, "routing")
	if err != nil {
		return cfg, err
	}
	if ov.Source == settings.SourceApp {
		var appDoc routingDoc
		if err := ov.Decode(&appDoc); err != nil {
			return cfg, err
		}
		cfg.App = appDoc.Rules
	}
	return cfg, nil
}

// Pick returns the eligible provider accounts for the request, best first
// (accounts whose breaker is open go last), or a core.ErrNoRoute /
// core.ErrCurrencyNotAccepted error.
func (r *Routing) Pick(ctx context.Context, q store.Queryer, req core.RouteRequest) ([]store.ProviderAccount, error) {
	app, err := store.GetApp(ctx, q, req.Scope.AppID)
	if err != nil {
		return nil, core.NewError(core.ErrNotFound, "app not found")
	}
	cfg, err := r.config(ctx, q, app)
	if err != nil {
		return nil, err
	}
	pas, err := store.ListAppProviderAccounts(ctx, q, app.ID, req.Scope.Mode)
	if err != nil {
		return nil, err
	}
	accounts := make([]routing.Account, 0, len(pas))
	byID := map[string]store.ProviderAccount{}
	for _, pa := range pas {
		accounts = append(accounts, routing.Account{ID: pa.ID, Connector: pa.Connector, Currencies: pa.AcceptedCurrencies,
			Purposes: pa.EnabledPurposes, Disabled: pa.Status != "active"})
		byID[pa.ID] = pa
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = routing.PurposeOneTime
	}
	var isOpen func(string) bool
	if r.breaker != nil {
		isOpen = r.breaker.Open
	}
	res, err := routing.Choose(cfg, routing.Request{Currency: req.Currency, Country: req.Country, Amount: req.Amount,
		Purpose: purpose, Metadata: req.Metadata, AllowedConnectors: req.AllowedConnectors}, accounts, isOpen)
	var rej *routing.RejectedError
	switch {
	case errors.As(err, &rej):
		msg := rej.Message
		if msg == "" {
			msg = "payments in " + req.Currency + " are not accepted"
		}
		return nil, core.NewError(core.ErrCurrencyNotAccepted, "%s", msg)
	case errors.Is(err, routing.ErrNoRoute):
		return nil, core.NewError(core.ErrNoRoute, "no payment provider can take %s payments for this app", req.Currency)
	case err != nil:
		return nil, err
	}
	out := make([]store.ProviderAccount, 0, len(res.Accounts))
	for _, id := range res.Accounts {
		out = append(out, byID[id])
	}
	return out, nil
}

// CheckRoute implements core.Router.
func (r *Routing) CheckRoute(ctx context.Context, q store.Queryer, req core.RouteRequest) error {
	_, err := r.Pick(ctx, q, req)
	return err
}
