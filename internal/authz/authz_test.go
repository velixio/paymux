package authz

import "testing"

func TestRoleMatrix(t *testing.T) {
	type row struct {
		action Action
		admin  bool
		oper   bool
		viewer bool
	}
	rows := []row{
		{PaymentRead, true, true, true},
		{PaymentCreate, true, true, false},
		{PaymentCancel, true, true, false},
		{RefundRead, true, true, true},
		{RefundCreate, true, true, false},
		{DisputeManage, true, true, false},
		{WebhookReplay, true, true, false},
		{InvoiceReissue, true, true, false},
		{SettingsRead, true, true, true},
		{SettingsWrite, true, false, false},
		{AppCreate, true, false, false},
		{APIKeyCreate, true, false, false},
		{ProviderAccountManage, true, false, false},
		{UserManage, true, false, false},
		{AuditRead, true, false, false},
		{RoleManage, true, false, false},
	}
	if len(rows) != len(AllActions) {
		t.Fatalf("matrix has %d rows for %d actions", len(rows), len(AllActions))
	}
	for _, r := range rows {
		for role, want := range map[Role]bool{RoleAdmin: r.admin, RoleOperator: r.oper, RoleViewer: r.viewer} {
			a := Actor{Kind: KindUser, ID: "usr_1", Role: role}
			if got := Can(a, r.action, Resource{}); got != want {
				t.Errorf("%s %s: got %v want %v", role, r.action, got, want)
			}
		}
	}
}

func TestUnknownActionAndRoleDenied(t *testing.T) {
	if Can(Actor{Kind: KindUser, Role: RoleAdmin}, Action("payment.explode"), Resource{}) {
		t.Error("unknown action allowed")
	}
	if Can(Actor{Kind: KindUser, Role: Role("root")}, PaymentRead, Resource{}) {
		t.Error("unknown role allowed")
	}
	if Can(Actor{}, PaymentRead, Resource{}) {
		t.Error("zero actor allowed")
	}
	if Can(Actor{Kind: KindSystem}, Action("payment.explode"), Resource{}) {
		t.Error("system actor must still be bound by the closed enum")
	}
}

func TestSystemActorAllowed(t *testing.T) {
	for _, a := range AllActions {
		if !Can(Actor{Kind: KindSystem}, a, Resource{AppID: "app_1"}) {
			t.Errorf("system denied %s", a)
		}
	}
}

func TestAPIKeyScopedToItsApp(t *testing.T) {
	key := Actor{Kind: KindAPIKey, ID: "key_1", AppID: "app_1", Mode: "test"}
	allowed := []Action{PaymentCreate, PaymentRead, PaymentCancel, RefundCreate, RefundRead}
	for _, a := range AllActions {
		isAllowed := false
		for _, x := range allowed {
			isAllowed = isAllowed || x == a
		}
		if got := Can(key, a, Resource{AppID: "app_1", Mode: "test"}); got != isAllowed {
			t.Errorf("api key %s on own app: got %v want %v", a, got, isAllowed)
		}
		if Can(key, a, Resource{AppID: "app_2", Mode: "test"}) {
			t.Errorf("api key %s allowed on another app", a)
		}
		if Can(key, a, Resource{AppID: "app_1", Mode: "live"}) {
			t.Errorf("api key %s allowed across modes", a)
		}
	}
	if Can(key, PaymentRead, Resource{}) {
		t.Error("api key allowed on a resource with no app")
	}
	if Can(Actor{Kind: KindAPIKey, Mode: "test"}, PaymentRead, Resource{Mode: "test"}) {
		t.Error("api key without an app allowed")
	}
}

func TestUserOrgScoping(t *testing.T) {
	u := Actor{Kind: KindUser, Role: RoleAdmin, OrgID: "org_1"}
	if !Can(u, PaymentRead, Resource{OrgID: "org_1"}) {
		t.Error("same org denied")
	}
	if Can(u, PaymentRead, Resource{OrgID: "org_2"}) {
		t.Error("other org allowed")
	}
}
