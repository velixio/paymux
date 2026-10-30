// Package checkout is the integration layer between core, the connector
// runtime and routing: it routes payments, starts provider checkouts, syncs
// attempts and refunds from the provider's truth, and runs the inbound,
// refund-submit and polling jobs.
package checkout

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/routing"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// Routing evaluates the routing setting (app rules, org rules, catch-all)
// against the app's provider accounts in the payment's mode. It implements
// core.Router for create-time validation; the account is picked later, on
// the pay page, with the circuit breaker applied.
type Routing struct {
	breaker  *routing.Breaker
	settings *settings.Service // when set, routing rules come through its cache
	suggest  Suggester
}

var _ core.Router = (*Routing)(nil)

// NewRouting returns a Routing using b to deprioritise failing accounts.
func NewRouting(b *routing.Breaker) *Routing { return &Routing{breaker: b} }

// WithSettings makes Routing read the routing setting through s's short-TTL
// cache instead of querying it for every payment. It returns r.
func (r *Routing) WithSettings(s *settings.Service) *Routing { r.settings = s; return r }

// Suggester proposes an order for the eligible provider accounts of a payment
// (the Go hook OnRoute). It returns provider account ids, or connector names
// meaning every candidate of that connector, best first. It can only reorder
// candidates routing already allows: it cannot add an account, and accounts
// whose circuit breaker is open still go last.
type Suggester func(ctx context.Context, req core.RouteRequest, candidates []store.ProviderAccount) []string

// WithSuggester makes Pick ask f for an order. A panic in f is ignored and
// leaves the order unchanged. It returns r.
func (r *Routing) WithSuggester(f Suggester) *Routing { r.suggest = f; return r }

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
		a := routing.Account{ID: pa.ID, Connector: pa.Connector, Currencies: pa.AcceptedCurrencies,
			Purposes: pa.EnabledPurposes, Disabled: pa.Status != "active"}
		if _, info, ok := connector.Lookup(pa.Connector); ok {
			a.SubscriptionCurrencies = info.SubscriptionCurrencies // a connector may bill subscriptions in fewer currencies
		}
		accounts = append(accounts, a)
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
	out, err := r.pick(ctx, q, req)
	if err != nil || r.suggest == nil || len(out) < 2 {
		return out, err
	}
	return r.applySuggestion(ctx, req, out), nil
}

// applySuggestion reorders accounts as the suggester asks, then puts accounts
// with an open breaker back at the end (keeping their relative order).
func (r *Routing) applySuggestion(ctx context.Context, req core.RouteRequest, accounts []store.ProviderAccount) (out []store.ProviderAccount) {
	defer func() {
		if recover() != nil {
			out = accounts
		}
	}()
	want := r.suggest(ctx, req, slices.Clone(accounts))
	if len(want) == 0 {
		return accounts
	}
	placed := map[string]bool{}
	ordered := make([]store.ProviderAccount, 0, len(accounts))
	for _, w := range want {
		for _, pa := range accounts {
			if !placed[pa.ID] && (pa.ID == w || pa.Connector == w) {
				placed[pa.ID] = true
				ordered = append(ordered, pa)
			}
		}
	}
	for _, pa := range accounts {
		if !placed[pa.ID] {
			ordered = append(ordered, pa)
		}
	}
	if r.breaker == nil {
		return ordered
	}
	out = make([]store.ProviderAccount, 0, len(ordered))
	var open []store.ProviderAccount
	for _, pa := range ordered {
		if r.breaker.Open(pa.ID) {
			open = append(open, pa)
		} else {
			out = append(out, pa)
		}
	}
	return append(out, open...)
}

func (r *Routing) pick(ctx context.Context, q store.Queryer, req core.RouteRequest) ([]store.ProviderAccount, error) {
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
		if fb := testOnlyFallback(req, purpose, pas); len(fb) > 0 {
			return fb, nil
		}
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
	_, err := r.pick(ctx, q, req)
	return err
}

// testOnlyFallback is what takes a test-mode payment that no rule routed to
// any account: the app's active accounts of test-only connectors (the
// simulator) that accept the currency and purpose. It makes "link a simulator
// account" enough to try the whole flow, without writing a routing rule. It is
// only used when routing found nothing; a rule that matches, or one that
// rejects, is never overridden.
func testOnlyFallback(req core.RouteRequest, purpose string, pas []store.ProviderAccount) []store.ProviderAccount {
	if req.Scope.Mode != "test" {
		return nil
	}
	var out []store.ProviderAccount
	for _, pa := range pas {
		_, info, ok := connector.Lookup(pa.Connector)
		if !ok || !info.TestOnly || pa.Status != "active" || pa.Mode != "test" {
			continue
		}
		if len(req.AllowedConnectors) > 0 && !slices.Contains(req.AllowedConnectors, pa.Connector) {
			continue
		}
		if !slices.ContainsFunc(pa.AcceptedCurrencies, func(c string) bool { return strings.EqualFold(c, req.Currency) }) || !slices.Contains(pa.EnabledPurposes, purpose) {
			continue
		}
		out = append(out, pa)
	}
	return out
}
