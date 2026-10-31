// Package routing chooses provider accounts for a payment from layered,
// data-driven rule sets (app rules, org rules, org catch-all), then filters
// by account capability and health. It is pure and has no I/O.
package routing

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// PeriodStart returns the UTC boundary used by routing quota periods.
func PeriodStart(period string, now time.Time) (time.Time, error) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "daily":
		return day, nil
	case "weekly":
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset), nil
	case "monthly":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	default:
		return time.Time{}, fmt.Errorf("routing: unknown quota period %q", period)
	}
}

// Purposes a request can have.
const (
	PurposeOneTime      = "one_time"
	PurposeSubscription = "subscription"
)

// When holds a rule's conditions; empty fields match anything. All set fields must match.
type When struct {
	Currency  []string          `json:"currency,omitempty"`
	Country   []string          `json:"country,omitempty"`
	AmountMin *int64            `json:"amount_min,omitempty"`
	AmountMax *int64            `json:"amount_max,omitempty"`
	Purpose   []string          `json:"purpose,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Target is a connector name (any eligible account of it) or a pinned account id.
type Target struct {
	Connector string `json:"connector,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}

// Action is what a matching rule does.
type Action struct {
	Kind    string   `json:"kind"` // "route" | "reject"
	Targets []Target `json:"targets,omitempty"`
	Message string   `json:"message,omitempty"`
}

// Limit makes a routing rule stop matching after its target accounts have
// processed the configured number or value of successful sales in the current
// UTC day, week or month. Nil limits are unlimited.
type Limit struct {
	Period    string `json:"period,omitempty"` // daily | weekly | monthly
	MaxSales  *int64 `json:"max_sales,omitempty"`
	MaxAmount *int64 `json:"max_amount,omitempty"`
}

// Route builds a route action over connector names.
func Route(connectors ...string) Action {
	a := Action{Kind: "route"}
	for _, c := range connectors {
		a.Targets = append(a.Targets, Target{Connector: c})
	}
	return a
}

// RouteAccounts builds a route action over pinned account ids.
func RouteAccounts(accountIDs ...string) Action {
	a := Action{Kind: "route"}
	for _, id := range accountIDs {
		a.Targets = append(a.Targets, Target{AccountID: id})
	}
	return a
}

// Reject builds a reject action with a customer-facing message.
func Reject(message string) Action { return Action{Kind: "reject", Message: message} }

// Rule is one ordered routing rule.
type Rule struct {
	Name   string `json:"name"`
	When   When   `json:"when"`
	Action Action `json:"action"`
	Limit  *Limit `json:"limit,omitempty"`
}

// Config is the full rule hierarchy for one app.
type Config struct {
	App      []Rule `json:"app,omitempty"`
	Org      []Rule `json:"org,omitempty"`
	CatchAll Rule   `json:"catch_all"`
}

// Account is a provider account the app may use, in the request's mode.
type Account struct {
	ID         string
	Connector  string
	Currencies []string // accepted_currencies
	// SubscriptionCurrencies, when set, limits the currencies the account takes
	// for the subscription purpose (a connector may take more for one-time payments).
	SubscriptionCurrencies []string
	Purposes               []string // enabled_purposes
	Disabled               bool
}

// Request describes what needs routing.
type Request struct {
	Currency          string
	Country           string
	Amount            int64
	Purpose           string
	Metadata          map[string]string
	AllowedConnectors []string
	// Usage returns successful sales and amount for the supplied provider
	// accounts since the current period began. Nil means limits cannot be used.
	Usage func(accountIDs []string, period string) (sales, amount int64, err error)
}

// Result is the chosen account order and the rule that produced it.
type Result struct {
	Accounts []string
	Rule     string
}

// ErrNoRoute means no rule produced an eligible account.
var ErrNoRoute = errors.New("routing: no route")

// RejectedError is returned when a matching rule rejects the request.
type RejectedError struct{ Rule, Message string }

func (e *RejectedError) Error() string {
	return fmt.Sprintf("routing: rejected by rule %q: %s", e.Rule, e.Message)
}

func (w When) matches(r Request) bool {
	if len(w.Currency) > 0 && !slices.Contains(w.Currency, r.Currency) ||
		len(w.Country) > 0 && !slices.Contains(w.Country, r.Country) ||
		len(w.Purpose) > 0 && !slices.Contains(w.Purpose, r.Purpose) ||
		w.AmountMin != nil && r.Amount < *w.AmountMin ||
		w.AmountMax != nil && r.Amount > *w.AmountMax {
		return false
	}
	for k, v := range w.Metadata {
		if r.Metadata[k] != v {
			return false
		}
	}
	return true
}

func eligible(a Account, r Request) bool {
	return !a.Disabled &&
		slices.Contains(a.Currencies, r.Currency) &&
		(r.Purpose != PurposeSubscription || len(a.SubscriptionCurrencies) == 0 || slices.Contains(a.SubscriptionCurrencies, r.Currency)) &&
		(r.Purpose == "" || slices.Contains(a.Purposes, r.Purpose)) &&
		(len(r.AllowedConnectors) == 0 || slices.Contains(r.AllowedConnectors, a.Connector))
}

func resolve(targets []Target, r Request, accounts []Account) []string {
	var out []string
	add := func(id string) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, t := range targets {
		for _, a := range accounts {
			if (t.AccountID != "" && a.ID == t.AccountID || t.AccountID == "" && a.Connector == t.Connector) && eligible(a, r) {
				add(a.ID)
			}
		}
	}
	return out
}

// Choose evaluates app rules, org rules, then the catch-all. A matching rule
// whose targets are all filtered out falls through to the next matching rule.
// isOpen reports an open circuit breaker; such accounts sort last.
func Choose(cfg Config, r Request, accounts []Account, isOpen func(accountID string) bool) (Result, error) {
	rules := append(append(append([]Rule{}, cfg.App...), cfg.Org...), cfg.CatchAll)
	for _, rule := range rules {
		if rule.Action.Kind == "" || !rule.When.matches(r) {
			continue
		}
		if rule.Action.Kind == "reject" {
			return Result{}, &RejectedError{Rule: rule.Name, Message: rule.Action.Message}
		}
		ids := resolve(rule.Action.Targets, r, accounts)
		if len(ids) == 0 {
			continue
		}
		if rule.Limit != nil && (rule.Limit.MaxSales != nil || rule.Limit.MaxAmount != nil) {
			if r.Usage == nil {
				continue
			}
			sales, amount, err := r.Usage(ids, rule.Limit.Period)
			if err != nil {
				return Result{}, err
			}
			// Include the payment currently being routed, so a max of one sale
			// really admits only the first sale in the period.
			if rule.Limit.MaxSales != nil && sales+1 > *rule.Limit.MaxSales ||
				rule.Limit.MaxAmount != nil && amount+r.Amount > *rule.Limit.MaxAmount {
				continue
			}
		}
		if isOpen != nil {
			slices.SortStableFunc(ids, func(a, b string) int {
				oa, ob := isOpen(a), isOpen(b)
				switch {
				case oa == ob:
					return 0
				case oa:
					return 1
				}
				return -1
			})
		}
		return Result{Accounts: ids, Rule: rule.Name}, nil
	}
	return Result{}, fmt.Errorf("%w for currency %s purpose %s", ErrNoRoute, r.Currency, r.Purpose)
}
