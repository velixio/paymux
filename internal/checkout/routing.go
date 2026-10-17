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
type Routing struct {
	breaker  *routing.Breaker
	settings *settings.Service // when set, routing rules come through its cache
}

var _ core.Router = (*Routing)(nil)

// NewRouting returns a Routing using b to deprioritise failing accounts.
func NewRouting(b *routing.Breaker) *Routing { return &Routing{breaker: b} }

// WithSettings makes Routing read the routing setting through s's short-TTL
// cache instead of querying it for every payment. It returns r.
func (r *Routing) WithSettings(s *settings.Service) *Routing { r.settings = s; return r }

// ruleSource reads a routing setting for an org or an app.
type ruleSource struct {
	org func(orgID string) (settings.Value, error)
	app func(appID string) (settings.Value, error)
}

func directSource(ctx context.Context, q store.Queryer) ruleSource {
	return ruleSource{
		org: func(orgID string) (settings.Value, error) { return settings.ResolveOrgWith(ctx, q, orgID, "routing") },
		app: func(appID string) (settings.Value, error) { return settings.ResolveWith(ctx, q, appID, "routing") },
	}
}

func cachedSource(ctx context.Context, q store.Queryer, s *settings.Service) ruleSource {
	return ruleSource{
		org: func(orgID string) (settings.Value, error) { return s.ResolveOrgCachedWith(ctx, q, orgID, "routing") },
		app: func(appID string) (settings.Value, error) { return s.ResolveCachedWith(ctx, q, appID, "routing") },
	}
}

type routingDoc struct {
	Rules    []routing.Rule `json:"rules"`
	CatchAll *routing.Rule  `json:"catch_all"`
}

// OrgRoutingConfig loads the org-level routing setting: the org rules and the
// catch-all.
func OrgRoutingConfig(ctx context.Context, q store.Queryer, orgID string) (routing.Config, error) {
	return orgRoutingConfig(directSource(ctx, q), orgID)
}

func orgRoutingConfig(src ruleSource, orgID string) (routing.Config, error) {
	var cfg routing.Config
	org, err := src.org(orgID)
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
	return cfg, nil
}

// AppRoutingConfig loads the rule hierarchy for an app: its own override (if it
// has one) ahead of the org rules and the org catch-all. Payments are routed
// with exactly this configuration.
func AppRoutingConfig(ctx context.Context, q store.Queryer, app store.App) (routing.Config, error) {
	return appRoutingConfig(directSource(ctx, q), app)
}

func appRoutingConfig(src ruleSource, app store.App) (routing.Config, error) {
	cfg, err := orgRoutingConfig(src, app.OrgID)
	if err != nil {
		return cfg, err
	}
	ov, err := src.app(app.ID)
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

// RoutingAccounts converts provider accounts to what routing.Choose evaluates.
func RoutingAccounts(pas []store.ProviderAccount) []routing.Account {
	accounts := make([]routing.Account, 0, len(pas))
	for _, pa := range pas {
		accounts = append(accounts, routing.Account{ID: pa.ID, Connector: pa.Connector, Currencies: pa.AcceptedCurrencies,
			Purposes: pa.EnabledPurposes, Disabled: pa.Status != "active"})
	}
	return accounts
}

func (r *Routing) config(ctx context.Context, q store.Queryer, app store.App) (routing.Config, error) {
	if r.settings != nil {
		return appRoutingConfig(cachedSource(ctx, q, r.settings), app)
	}
	return AppRoutingConfig(ctx, q, app)
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
	accounts := RoutingAccounts(pas)
	byID := map[string]store.ProviderAccount{}
	for _, pa := range pas {
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
