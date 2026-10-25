package authz

import (
	"slices"
	"testing"
)

// settings.admin guards the sign-in policy and the e-invoice trust anchor
// (audit H-3): only the built-in admin holds it, never a custom role.
func TestSettingsAdminIsNeverGrantable(t *testing.T) {
	if !ValidAction(SettingsAdmin) || slices.Contains(Grantable(), SettingsAdmin) {
		t.Fatal("settings.admin must be a valid action and not grantable")
	}
	r := fixedRoles{"clerk": {SettingsRead, SettingsWrite, SettingsAdmin}}
	user := func(role Role) Actor { return Actor{Kind: KindUser, ID: "u", Role: role} }
	for role, want := range map[Role]bool{RoleAdmin: true, RoleOperator: false, RoleViewer: false, "clerk": false} {
		if got := CanWith(r, user(role), SettingsAdmin, Resource{}); got != want {
			t.Errorf("%s: got %v want %v", role, got, want)
		}
	}
	if !CanWith(r, user("clerk"), SettingsWrite, Resource{}) {
		t.Error("a custom role keeps settings.write")
	}
}
