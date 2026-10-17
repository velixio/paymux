package store_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestRolesLifecycle(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		now := time.UnixMilli(1_800_000_000_000).UTC()
		if err := db.SeedBuiltinRoles(ctx, []store.RoleRow{{Name: "admin", Actions: []string{"a"}}, {Name: "viewer", Actions: []string{"payment.read"}}}, now); err != nil {
			t.Fatal(err)
		}
		// Seeding is idempotent and rewrites the stored actions.
		if err := db.SeedBuiltinRoles(ctx, []store.RoleRow{{Name: "viewer", Actions: []string{"payment.read", "refund.read"}}}, now); err != nil {
			t.Fatal(err)
		}
		created, err := db.SaveRole(ctx, "auditor", []string{"audit.read"}, now)
		if err != nil || !created {
			t.Fatalf("create: %v %v", created, err)
		}
		if created, err = db.SaveRole(ctx, "auditor", []string{"audit.read", "payment.read"}, now.Add(time.Hour)); err != nil || created {
			t.Fatalf("update: %v %v", created, err)
		}
		if _, err := db.SaveRole(ctx, "admin", nil, now); !errors.Is(err, store.ErrBuiltinRole) {
			t.Errorf("built-in overwritten: %v", err)
		}
		list, err := db.ListRoles(ctx)
		if err != nil || len(list) != 3 || list[0].Name != "admin" || !list[0].Builtin || list[1].Name != "viewer" ||
			list[2].Name != "auditor" || list[2].Builtin || !reflect.DeepEqual(list[2].Actions, []string{"audit.read", "payment.read"}) ||
			!reflect.DeepEqual(list[1].Actions, []string{"payment.read", "refund.read"}) || !list[2].UpdatedAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("list = %+v %v", list, err)
		}

		// Users can hold built-in and stored roles only.
		u := func(id, email, role string) store.UserRow {
			return store.UserRow{ID: id, Email: email, PasswordHash: "h", TOTPLastStep: -1, Role: role, CreatedAt: now}
		}
		if err := db.InsertUser(ctx, u("u1", "a@example.com", "auditor")); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertUser(ctx, u("u2", "b@example.com", "nonesuch")); !errors.Is(err, store.ErrUnknownRole) {
			t.Errorf("unknown role at insert: %v", err)
		}
		if err := db.InsertUser(ctx, u("u3", "c@example.com", "operator")); err != nil {
			t.Errorf("built-in not yet listed in the table: %v", err)
		}
		if err := db.SetUserRole(ctx, "u3", "nonesuch"); !errors.Is(err, store.ErrUnknownRole) {
			t.Errorf("unknown role at update: %v", err)
		}
		if err := db.SetUserRole(ctx, "u3", "auditor"); err != nil {
			t.Error(err)
		}
		if err := db.SetUserRole(ctx, "nobody", "auditor"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("missing user: %v", err)
		}
		counts, _ := db.RoleUserCounts(ctx)
		if !reflect.DeepEqual(counts, map[string]int{"auditor": 2}) {
			t.Errorf("counts = %v", counts)
		}

		if err := db.DeleteRole(ctx, "auditor"); !errors.Is(err, store.ErrRoleInUse) {
			t.Errorf("delete in use: %v", err)
		}
		if err := db.DeleteRole(ctx, "viewer"); !errors.Is(err, store.ErrBuiltinRole) {
			t.Errorf("delete built-in: %v", err)
		}
		_ = db.SetUserRole(ctx, "u1", "admin")
		_ = db.SetUserRole(ctx, "u3", "admin")
		if err := db.DeleteRole(ctx, "auditor"); err != nil {
			t.Errorf("delete: %v", err)
		}
		if err := db.DeleteRole(ctx, "auditor"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete again: %v", err)
		}
	})
}

func TestAPIKeyScopesAndExpiry(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		now := time.UnixMilli(1_800_000_000_000).UTC()
		key := func(id string, scopes []string, at time.Time) store.APIKey {
			return store.APIKey{ID: id, OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "live", Prefix: "pmx_live_sk_" + id, Hash: "h" + id, CreatedAt: at, Scopes: scopes}
		}
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, k := range []store.APIKey{key("k1", nil, now), key("k2", []string{"payments:read"}, now.Add(time.Second)), key("k3", []string{}, now.Add(2*time.Second))} {
				if err := store.InsertAPIKey(ctx, tx, k); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		get := func(id string) store.APIKey {
			var k store.APIKey
			_ = db.Read(ctx, func(q store.Querier) (err error) { k, err = store.GetAPIKeyByPrefix(ctx, q, "pmx_live_sk_"+id); return })
			return k
		}
		if k := get("k1"); k.Scopes != nil || !k.ExpiresAt.IsZero() {
			t.Errorf("all-scopes key: %+v", k)
		}
		if k := get("k2"); !reflect.DeepEqual(k.Scopes, []string{"payments:read"}) {
			t.Errorf("scoped key: %+v", k)
		}
		if k := get("k3"); k.Scopes == nil || len(k.Scopes) != 0 {
			t.Errorf("a key with no scopes must stay distinct from one with all: %#v", k.Scopes)
		}

		// Expire everything but k3; a key that already expires sooner is left alone.
		at := now.Add(24 * time.Hour)
		var changed []store.APIKey
		err = db.Write(ctx, func(tx store.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE id = 'k2'`, now.Add(time.Hour).UnixMilli()); err != nil {
				return err
			}
			changed, err = store.ExpireAPIKeys(ctx, tx, "app_a", "live", "k3", at)
			return err
		})
		if err != nil || len(changed) != 1 || changed[0].ID != "k1" {
			t.Fatalf("changed = %+v %v", changed, err)
		}
		if k := get("k1"); !k.ExpiresAt.Equal(at) {
			t.Errorf("k1 expires %v", k.ExpiresAt)
		}
		if k := get("k2"); !k.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Errorf("k2 expiry was pushed out: %v", k.ExpiresAt)
		}
		if k := get("k3"); !k.ExpiresAt.IsZero() {
			t.Errorf("the new key expires: %v", k.ExpiresAt)
		}
		var keys []store.APIKey
		_ = db.Read(ctx, func(q store.Querier) (err error) { keys, err = store.ListAPIKeys(ctx, q, "app_a"); return })
		if len(keys) != 3 || !reflect.DeepEqual(keys[1].Scopes, []string{"payments:read"}) || !keys[0].ExpiresAt.Equal(at) {
			t.Errorf("list = %+v", keys)
		}
	})
}
