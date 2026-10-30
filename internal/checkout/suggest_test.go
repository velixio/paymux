package checkout_test

import (
	"context"
	"slices"
	"testing"

	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/routing"
	"github.com/velixio/paymux/internal/store"
)

// pickIDs runs Routing.Pick with the given suggester and returns the account ids in order.
func pickIDs(t *testing.T, e *env, suggest checkout.Suggester) []string {
	t.Helper()
	rt := checkout.NewRouting(e.breaker).WithSettings(e.set).WithSuggester(suggest)
	var ids []string
	err := e.db.Read(ctx, func(q store.Querier) error {
		pas, err := rt.Pick(ctx, q, core.RouteRequest{Scope: e.scope, Currency: "INR", Amount: 50000})
		for _, pa := range pas {
			ids = append(ids, pa.ID)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestSuggesterReordersEligibleAccounts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		plain := pickIDs(t, e, nil)
		if len(plain) != 2 {
			t.Fatalf("expected two accounts, got %v", plain)
		}
		reversed := []string{plain[1], plain[0]}

		var seen core.RouteRequest
		var offered []string
		got := pickIDs(t, e, func(_ context.Context, req core.RouteRequest, candidates []store.ProviderAccount) []string {
			seen = req
			for _, c := range candidates {
				offered = append(offered, c.ID)
			}
			return reversed
		})
		if !slices.Equal(got, reversed) {
			t.Errorf("suggested order not applied: got %v want %v", got, reversed)
		}
		if seen.Currency != "INR" || seen.Amount != 50000 || seen.Scope.AppID != "app_1" || !slices.Equal(offered, plain) {
			t.Errorf("suggester saw %+v with candidates %v (default order %v)", seen, offered, plain)
		}

		// Unknown ids are ignored, and accounts it does not mention keep their place after the named ones.
		got = pickIDs(t, e, func(context.Context, core.RouteRequest, []store.ProviderAccount) []string {
			return []string{"pa_does_not_exist", plain[1]}
		})
		if !slices.Equal(got, reversed) {
			t.Errorf("partial suggestion: got %v want %v", got, reversed)
		}
		// A repeated id counts once, and an empty suggestion changes nothing.
		got = pickIDs(t, e, func(context.Context, core.RouteRequest, []store.ProviderAccount) []string {
			return []string{plain[1], plain[1]}
		})
		if len(got) != 2 {
			t.Errorf("duplicate id duplicated an account: %v", got)
		}
		if got := pickIDs(t, e, func(context.Context, core.RouteRequest, []store.ProviderAccount) []string { return nil }); !slices.Equal(got, plain) {
			t.Errorf("empty suggestion changed the order: %v", got)
		}
	})
}

func TestSuggesterCannotAddAccountsOrBeatTheBreaker(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		plain := pickIDs(t, e, nil)
		// Ask for the failing account first: its open breaker still wins.
		other := e.otherAccount(plain[0])
		for range routing.BreakerFailures {
			e.breaker.Failure(plain[0])
		}
		if !e.breaker.Open(plain[0]) {
			t.Fatal("breaker did not open")
		}
		got := pickIDs(t, e, func(context.Context, core.RouteRequest, []store.ProviderAccount) []string {
			return []string{plain[0], other}
		})
		// Both accounts are eligible; the one whose breaker is open stays last whatever was suggested.
		if len(got) != 2 || got[0] != other || got[1] != plain[0] {
			t.Errorf("breaker-open account must stay last: %v (open: %s)", got, plain[0])
		}
	})
}

func TestSuggesterPanicAndCreateTimeCheck(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		plain := pickIDs(t, e, nil)
		calls := 0
		// A panicking hook must not break routing.
		got := pickIDs(t, e, func(context.Context, core.RouteRequest, []store.ProviderAccount) []string {
			calls++
			panic("hook bug")
		})
		if !slices.Equal(got, plain) || calls != 1 {
			t.Errorf("after a panic: %v (calls %d), want %v", got, calls, plain)
		}
		// Create-time validation (CheckRoute) never asks the hook.
		rt := checkout.NewRouting(e.breaker).WithSettings(e.set).WithSuggester(func(context.Context, core.RouteRequest, []store.ProviderAccount) []string {
			calls++
			return nil
		})
		if err := e.db.Read(ctx, func(q store.Querier) error {
			return rt.CheckRoute(ctx, q, core.RouteRequest{Scope: e.scope, Currency: "INR", Amount: 50000})
		}); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Errorf("CheckRoute called the suggester (%d calls)", calls)
		}
	})
}
