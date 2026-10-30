package core_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
)

var orgScope = core.OrgScope{OrgID: store.DefaultOrgID}

func TestScopedKeysCarryTheirScopes(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		app, _ := e.svc.CreateApp(ctx, audit.System, orgScope, core.CreateAppInput{Name: "S", Slug: "scoped"})

		tok, key, err := e.svc.CreateAPIKeyWith(ctx, audit.System, orgScope, app.ID, "test",
			core.KeyOptions{Scopes: []string{"refunds:write", "payments:read", "payments:read"}})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"payments:read", "refunds:write"} // canonical order, no duplicates
		if !reflect.DeepEqual(key.Scopes, want) {
			t.Errorf("scopes = %v", key.Scopes)
		}
		pr, err := e.svc.Authenticate(ctx, tok)
		if err != nil || !reflect.DeepEqual(pr.Scopes, want) {
			t.Errorf("principal %+v %v", pr, err)
		}

		// A key made the old way, and one made before scopes existed, have all of them.
		tok2, _, _ := e.svc.CreateAPIKey(ctx, audit.System, orgScope, app.ID, "test")
		if pr, err := e.svc.Authenticate(ctx, tok2); err != nil || pr.Scopes != nil {
			t.Errorf("default key %+v %v", pr, err)
		}

		for name, bad := range map[string][]string{"unknown": {"payments:delete"}, "empty": {}, "mixed": {"payments:read", "nope"}} {
			if _, _, err := e.svc.CreateAPIKeyWith(ctx, audit.System, orgScope, app.ID, "test", core.KeyOptions{Scopes: bad}); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'api_key.create' AND after LIKE '%payments:read,refunds:write%'") != 1 {
			t.Error("the key's scopes are not in the audit log")
		}
	})
}

func TestRotateAPIKeyKeepsBothWorkingUntilTheGracePeriodEnds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		now := time.Unix(1_800_000_000, 0).UTC()
		svc := core.NewService(core.Options{DB: e.db, Settings: e.set, Jobs: e.q, Now: func() time.Time { return now }})
		app, _ := svc.CreateApp(ctx, audit.System, orgScope, core.CreateAppInput{Name: "R", Slug: "rotate"})
		other, _ := svc.CreateApp(ctx, audit.System, orgScope, core.CreateAppInput{Name: "O", Slug: "other"})

		oldTok, oldKey, _ := svc.CreateAPIKeyWith(ctx, audit.System, orgScope, app.ID, "live", core.KeyOptions{Scopes: []string{"payments:read"}})
		now = now.Add(time.Minute)
		oldTok2, oldKey2, _ := svc.CreateAPIKeyWith(ctx, audit.System, orgScope, app.ID, "live", core.KeyOptions{Scopes: []string{"invoices:read"}})
		testTok, _, _ := svc.CreateAPIKey(ctx, audit.System, orgScope, app.ID, "test")
		otherTok, _, _ := svc.CreateAPIKey(ctx, audit.System, orgScope, other.ID, "live")
		now = now.Add(time.Minute)

		newTok, newKey, expiring, err := svc.RotateAPIKey(ctx, audit.System, orgScope, app.ID, "live", 24*time.Hour, core.KeyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if newKey.Mode != "live" || len(expiring) != 2 || expiring[0].ID != oldKey.ID || expiring[1].ID != oldKey2.ID ||
			!expiring[0].ExpiresAt.Equal(now.Add(24*time.Hour)) {
			t.Fatalf("new %+v expiring %+v", newKey, expiring)
		}
		if !reflect.DeepEqual(newKey.Scopes, []string{"invoices:read"}) {
			t.Errorf("the new key should take the newest old key's scopes, got %v", newKey.Scopes)
		}

		// Both work during the grace period; keys of other modes and apps are untouched.
		for name, tok := range map[string]string{"new": newTok, "old": oldTok, "old2": oldTok2, "test key": testTok, "other app": otherTok} {
			if _, err := svc.Authenticate(ctx, tok); err != nil {
				t.Errorf("%s key during grace: %v", name, err)
			}
		}
		now = now.Add(24*time.Hour - time.Second)
		if _, err := svc.Authenticate(ctx, oldTok); err != nil {
			t.Errorf("old key one second before expiry: %v", err)
		}
		now = now.Add(time.Second)
		for name, tok := range map[string]string{"old": oldTok, "old2": oldTok2} {
			if _, err := svc.Authenticate(ctx, tok); !errors.Is(err, core.ErrUnauthenticated) {
				t.Errorf("%s key after the grace period: %v", name, err)
			}
		}
		for name, tok := range map[string]string{"new": newTok, "test key": testTok, "other app": otherTok} {
			if _, err := svc.Authenticate(ctx, tok); err != nil {
				t.Errorf("%s key after the grace period: %v", name, err)
			}
		}

		// Rotating again expires the new key only after the earlier expiries
		// (it must not extend a key that already expires sooner).
		_, _, again, err := svc.RotateAPIKey(ctx, audit.System, orgScope, app.ID, "live", 48*time.Hour, core.KeyOptions{Scopes: []string{"payments:write"}})
		if err != nil || len(again) != 1 || again[0].ID != newKey.ID {
			t.Fatalf("second rotation expiring %+v %v", again, err)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'api_key.rotate'") != 2 {
			t.Error("rotation not audited")
		}
		if _, _, _, err := svc.RotateAPIKey(ctx, audit.System, orgScope, app.ID, "live", 0, core.KeyOptions{}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("zero grace: %v", err)
		}
		if _, _, _, err := svc.RotateAPIKey(ctx, audit.System, orgScope, "app_nope", "live", time.Hour, core.KeyOptions{}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing app: %v", err)
		}
		// A rotation with no previous key just creates one with all scopes.
		fresh, _ := svc.CreateApp(ctx, audit.System, orgScope, core.CreateAppInput{Name: "F", Slug: "fresh"})
		_, k, exp, err := svc.RotateAPIKey(ctx, audit.System, orgScope, fresh.ID, "test", time.Hour, core.KeyOptions{})
		if err != nil || len(exp) != 0 || k.Scopes != nil {
			t.Errorf("first key by rotation: %+v %v %v", k, exp, err)
		}
	})
}
