package store_test

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func addUsers(t *testing.T, db *store.DB, emails ...string) {
	t.Helper()
	now := time.UnixMilli(1_800_000_000_000).UTC()
	for i, e := range emails {
		id := "usr_" + string(rune('1'+i))
		if err := db.InsertUser(context.Background(), store.UserRow{ID: id, Email: e, PasswordHash: "h", TOTPLastStep: -1, Role: "viewer", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
}

func setTheme(t *testing.T, db *store.DB, id, name string) {
	t.Helper()
	if err := db.Write(context.Background(), func(tx store.Tx) error { return store.SetUserTheme(context.Background(), tx, id, name) }); err != nil {
		t.Fatal(err)
	}
}

func userTheme(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	var got string
	if err := db.Read(context.Background(), func(q store.Querier) (err error) {
		got, err = store.UserTheme(context.Background(), q, id)
		return
	}); err != nil {
		t.Fatalf("UserTheme(%s): %v", id, err)
	}
	return got
}

func TestUserThemeSetGetAndClear(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		ctx := context.Background()
		addUsers(t, db, "a@example.com", "b@example.com")
		if got := userTheme(t, db, "usr_1"); got != "" {
			t.Fatalf("new users have no theme, got %q", got)
		}
		setTheme(t, db, "usr_1", "paddle")
		if got := userTheme(t, db, "usr_1"); got != "paddle" {
			t.Fatalf("got %q", got)
		}
		if got := userTheme(t, db, "usr_2"); got != "" {
			t.Fatalf("other users are unaffected, got %q", got)
		}
		setTheme(t, db, "usr_1", "")
		if got := userTheme(t, db, "usr_1"); got != "" {
			t.Fatalf("empty clears, got %q", got)
		}
		err := db.Write(ctx, func(tx store.Tx) error { return store.SetUserTheme(ctx, tx, "usr_nope", "paddle") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("set for a missing user: %v", err)
		}
		err = db.Read(ctx, func(q store.Querier) error { _, err := store.UserTheme(ctx, q, "usr_nope"); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get for a missing user: %v", err)
		}
	})
}

func TestThemeUsersListsEmailsOfWhoChoseIt(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		ctx := context.Background()
		addUsers(t, db, "zed@example.com", "amy@example.com", "bob@example.com")
		setTheme(t, db, "usr_1", "paddle")
		setTheme(t, db, "usr_2", "paddle")
		setTheme(t, db, "usr_3", "midnight")
		var got, none []string
		if err := db.Read(ctx, func(q store.Querier) (err error) {
			if got, err = store.ThemeUsers(ctx, q, store.DefaultOrgID, "paddle"); err != nil {
				return
			}
			none, err = store.ThemeUsers(ctx, q, store.DefaultOrgID, "stripe")
			return
		}); err != nil {
			t.Fatal(err)
		}
		if want := []string{"amy@example.com", "zed@example.com"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if len(none) != 0 {
			t.Fatalf("nobody chose stripe: %v", none)
		}
	})
}

func TestRenameThemeRefsUpdatesTheOrgDefaultAndUserChoices(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		ctx := context.Background()
		now := time.UnixMilli(1_800_000_000_000).UTC()
		addUsers(t, db, "a@example.com", "b@example.com", "c@example.com")
		setTheme(t, db, "usr_1", "mine")
		setTheme(t, db, "usr_2", "mine")
		setTheme(t, db, "usr_3", "paddle")
		if err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SetOrgSetting(ctx, tx, store.DefaultOrgID, "theme", `"mine"`, now); err != nil {
				return err
			}
			// Another organisation's default of the same name is not ours to rename.
			return store.SetOrgSetting(ctx, tx, "org_other", "theme", `"mine"`, now)
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.Write(ctx, func(tx store.Tx) error {
			return store.RenameThemeRefs(ctx, tx, store.DefaultOrgID, "mine", "ours")
		}); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string]string{"usr_1": "ours", "usr_2": "ours", "usr_3": "paddle"} {
			if got := userTheme(t, db, id); got != want {
				t.Errorf("%s = %q, want %q", id, got, want)
			}
		}
		get := func(org string) string {
			var v string
			if err := db.Read(ctx, func(q store.Querier) (err error) {
				v, _, err = store.GetOrgSetting(ctx, q, org, "theme")
				return
			}); err != nil {
				t.Fatal(err)
			}
			return v
		}
		if got := get(store.DefaultOrgID); got != `"ours"` {
			t.Errorf("org default = %s", got)
		}
		if got := get("org_other"); got != `"mine"` {
			t.Errorf("another org's default changed: %s", got)
		}
		// An org default of a different theme is left alone.
		if err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SetOrgSetting(ctx, tx, store.DefaultOrgID, "theme", `"paddle"`, now); err != nil {
				return err
			}
			return store.RenameThemeRefs(ctx, tx, store.DefaultOrgID, "ours", "theirs")
		}); err != nil {
			t.Fatal(err)
		}
		if got := get(store.DefaultOrgID); got != `"paddle"` {
			t.Errorf("unrelated default changed: %s", got)
		}
	})
}

// 0034 adds users.theme: users that exist before it have no theme.
func TestUserThemeColumnIsAddedWithoutTouchingExistingUsers(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		before := fstest.MapFS{}
		for _, dir := range []string{"sqlite", "postgres"} {
			entries, _ := fs.ReadDir(migrations.FS, dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "0034_") {
					continue
				}
				b, _ := fs.ReadFile(migrations.FS, dir+"/"+e.Name())
				before[dir+"/"+e.Name()] = &fstest.MapFile{Data: b}
			}
		}
		if err := db.Migrate(ctx, before); err != nil {
			t.Fatal(err)
		}
		addUsers(t, db, "old@example.com")
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		if got := userTheme(t, db, "usr_1"); got != "" {
			t.Fatalf("existing user got theme %q", got)
		}
		u, err := db.UserByID(ctx, "usr_1")
		if err != nil || u.Email != "old@example.com" {
			t.Fatalf("user row after migration: %+v %v", u, err)
		}
	})
}
