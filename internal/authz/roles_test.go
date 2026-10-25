package authz

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type fixedRoles map[Role][]Action

func (f fixedRoles) Actions(r Role) ([]Action, bool) { a, ok := f[r]; return a, ok }

func TestCustomRolesResolveThroughTheResolver(t *testing.T) {
	r := fixedRoles{"auditor": {AuditRead, PaymentRead, UserManage, RoleManage}, "viewer": {UserManage}, "admin": {}}
	user := func(role Role) Actor { return Actor{Kind: KindUser, ID: "u", Role: role} }

	if !CanWith(r, user("auditor"), AuditRead, Resource{}) || !CanWith(r, user("auditor"), PaymentRead, Resource{}) {
		t.Error("granted actions denied")
	}
	if CanWith(r, user("auditor"), RefundCreate, Resource{}) {
		t.Error("an action the role lacks was allowed")
	}
	if CanWith(r, user("auditor"), UserManage, Resource{}) || CanWith(r, user("auditor"), RoleManage, Resource{}) {
		t.Error("admin-only actions must never be granted to a custom role, even if the table lists them")
	}
	if CanWith(r, user("ghost"), PaymentRead, Resource{}) {
		t.Error("a role the resolver does not know was allowed")
	}
	if CanWith(nil, user("auditor"), PaymentRead, Resource{}) {
		t.Error("no resolver: custom roles must be denied")
	}
	// Built-in roles ignore the resolver: the table cannot change them.
	if CanWith(r, user("viewer"), UserManage, Resource{}) || !CanWith(r, user("viewer"), PaymentRead, Resource{}) {
		t.Error("the viewer role was altered through the resolver")
	}
	if !CanWith(r, user("admin"), RoleManage, Resource{}) {
		t.Error("admin lost an action through the resolver")
	}
	if !CanWith(r, user("auditor"), AuditRead, Resource{OrgID: "o"}) || CanWith(r, Actor{Kind: KindUser, Role: "auditor", OrgID: "a"}, AuditRead, Resource{OrgID: "b"}) {
		t.Error("org check bypassed for custom roles")
	}
	if CanWith(r, Actor{Kind: KindAPIKey, Role: "auditor", AppID: "a"}, AuditRead, Resource{AppID: "a"}) {
		t.Error("an API key took a role")
	}
}

func TestGrantableExcludesAdminOnlyActions(t *testing.T) {
	g := Grantable()
	if slices.Contains(g, UserManage) || slices.Contains(g, RoleManage) || slices.Contains(g, SettingsAdmin) || slices.Contains(g, SystemUpdate) || len(g) != len(AllActions)-4 {
		t.Errorf("grantable = %v", g)
	}
	if !ValidAction(PaymentRead) || ValidAction("payment.explode") {
		t.Error("ValidAction")
	}
	b := BuiltinRoles()
	if len(b[RoleAdmin]) != len(AllActions) || !slices.Equal(b[RoleViewer], []Action{PaymentRead, RefundRead, SettingsRead}) || !slices.Contains(b[RoleOperator], RefundCreate) {
		t.Errorf("builtin = %v", b)
	}
}

func TestCacheReloadsAfterTTLAndInvalidate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var loads atomic.Int32
	data := map[Role][]Action{"auditor": {AuditRead}}
	var failing bool
	c := NewCache(func(context.Context) (map[Role][]Action, error) {
		loads.Add(1)
		if failing {
			return nil, errors.New("db down")
		}
		return data, nil
	}, 5*time.Second, func() time.Time { return now })

	if a, ok := c.Actions("auditor"); !ok || !slices.Equal(a, []Action{AuditRead}) {
		t.Fatalf("first lookup: %v %v", a, ok)
	}
	c.Actions("auditor")
	c.Actions("ghost")
	if loads.Load() != 1 {
		t.Errorf("loads = %d, want 1 within the TTL", loads.Load())
	}

	data = map[Role][]Action{"auditor": {AuditRead, PaymentRead}}
	if a, _ := c.Actions("auditor"); len(a) != 1 {
		t.Error("a change was seen before the TTL or an invalidation")
	}
	now = now.Add(6 * time.Second)
	if a, _ := c.Actions("auditor"); len(a) != 2 || loads.Load() != 2 {
		t.Errorf("after the TTL: %v loads=%d", a, loads.Load())
	}

	data = map[Role][]Action{}
	c.Invalidate()
	if _, ok := c.Actions("auditor"); ok || loads.Load() != 3 {
		t.Errorf("after Invalidate: ok=%v loads=%d", ok, loads.Load())
	}

	// A failing loader keeps serving the last good data and retries soon.
	data = map[Role][]Action{"auditor": {AuditRead}}
	c.Invalidate()
	c.Actions("auditor")
	failing = true
	now = now.Add(6 * time.Second)
	if a, ok := c.Actions("auditor"); !ok || len(a) != 1 {
		t.Errorf("stale data dropped on a loader error: %v %v", a, ok)
	}
	before := loads.Load()
	now = now.Add(2 * time.Second)
	c.Actions("auditor")
	if loads.Load() <= before {
		t.Error("no retry after a failed load")
	}
	// With no data at all, a failing loader denies everything.
	c2 := NewCache(func(context.Context) (map[Role][]Action, error) { return nil, errors.New("down") }, time.Second, nil)
	if _, ok := c2.Actions("auditor"); ok {
		t.Error("a role resolved with no data")
	}
}

func TestScopes(t *testing.T) {
	for _, s := range AllScopes {
		if !ValidScope(s) {
			t.Errorf("%s invalid", s)
		}
	}
	if ValidScope("payments:delete") || ValidScope("") {
		t.Error("unknown scope valid")
	}
	cases := []struct {
		granted []string
		need    string
		want    bool
	}{
		{nil, ScopeRefundsWrite, true},  // existing keys: every scope...
		{nil, ScopePayoutsWrite, false}, // ...except sending money out
		{nil, ScopePayoutsRead, false},
		{[]string{ScopePayoutsWrite}, ScopePayoutsWrite, true},
		{[]string{ScopePayoutsWrite}, ScopePayoutsRead, true}, // write covers read
		{[]string{ScopePayoutsRead}, ScopePayoutsWrite, false},
		{[]string{ScopePaymentsWrite, ScopeRefundsWrite}, ScopePayoutsWrite, false},
		{[]string{}, ScopePaymentsRead, false},                   // no scopes at all
		{[]string{ScopePaymentsRead}, ScopePaymentsRead, true},   // exact
		{[]string{ScopePaymentsRead}, ScopePaymentsWrite, false}, // read does not give write
		{[]string{ScopePaymentsWrite}, ScopePaymentsRead, true},  // write covers read
		{[]string{ScopePaymentsWrite}, ScopeRefundsRead, false},  // not across resources
		{[]string{ScopeInvoicesRead}, ScopeSubscriptionsRead, false},
		{[]string{ScopeRefundsWrite}, ScopeRefundsRead, true},
		{[]string{ScopePaymentsRead}, "", true}, // a route that needs no scope
	}
	for _, c := range cases {
		if got := ScopeAllows(c.granted, c.need); got != c.want {
			t.Errorf("ScopeAllows(%v, %q) = %v", c.granted, c.need, got)
		}
	}
}
