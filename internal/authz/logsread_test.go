package authz

import (
	"slices"
	"testing"
)

// logs.read is admin only by default, and a custom role can be given it.
func TestLogsReadIsAdminOnlyUnlessGranted(t *testing.T) {
	if !ValidAction(LogsRead) || !slices.Contains(Grantable(), LogsRead) {
		t.Fatal("logs.read must be a valid, grantable action")
	}
	r := fixedRoles{"support": {PaymentRead, LogsRead}, "finance": {PaymentRead}}
	user := func(role Role) Actor { return Actor{Kind: KindUser, ID: "u", Role: role} }
	for role, want := range map[Role]bool{RoleAdmin: true, RoleOperator: false, RoleViewer: false, "support": true, "finance": false, "ghost": false} {
		if got := CanWith(r, user(role), LogsRead, Resource{}); got != want {
			t.Errorf("%s: got %v want %v", role, got, want)
		}
	}
	if CanWith(r, Actor{Kind: KindAPIKey, AppID: "a", Mode: "test"}, LogsRead, Resource{AppID: "a"}) {
		t.Error("an API key must never read the logs")
	}
}
