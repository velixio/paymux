package look_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/look"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/web"
)

var ctx = context.Background()

func TestResolveLookForAnApp(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SeedOrg(ctx, tx, "Paymux", time.Now()); err != nil {
				return err
			}
			for _, id := range []string{"app_1", "app_2"} {
				if err := store.InsertApp(ctx, tx, store.App{ID: id, OrgID: store.DefaultOrgID, Name: id, Slug: id, CreatedAt: time.Now()}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		set := settings.New(db)
		rend, err := web.NewRenderer(web.Embedded())
		if err != nil {
			t.Fatal(err)
		}
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		actor := audit.System

		got := look.Resolve(ctx, set, rend, "app_1", "")
		if got.Theme != "default" || got.Lang != "en" {
			t.Fatalf("defaults: %+v", got)
		}

		must(set.SetOrg(ctx, actor, store.DefaultOrgID, "theme", json.RawMessage(`"minimal-dark"`)))
		must(set.SetApp(ctx, actor, store.DefaultOrgID, "app_1", "customer_language", json.RawMessage(`"hi"`)))
		must(set.SetApp(ctx, actor, store.DefaultOrgID, "app_1", "theme", json.RawMessage(`"default"`)))

		if got := look.Resolve(ctx, set, rend, "app_1", ""); got.Theme != "default" || got.Lang != "hi" {
			t.Fatalf("app values win: %+v", got)
		}
		if got := look.Resolve(ctx, set, rend, "app_2", ""); got.Theme != "minimal-dark" || got.Lang != "en" {
			t.Fatalf("an app without its own values inherits the org's: %+v", got)
		}
		// ?lang= wins, but only for installed catalogs.
		if got := look.Resolve(ctx, set, rend, "app_2", "hi"); got.Lang != "hi" {
			t.Fatalf("?lang=hi: %+v", got)
		}
		for _, bad := range []string{"zz", "../hi", "HI", "hi.json", "<script>"} {
			if got := look.Resolve(ctx, set, rend, "app_1", bad); got.Lang != "hi" {
				t.Fatalf("?lang=%q must be ignored, got %+v", bad, got)
			}
		}
		// A configured language nobody installed shows English.
		must(set.SetApp(ctx, actor, store.DefaultOrgID, "app_2", "customer_language", json.RawMessage(`"fr"`)))
		if got := look.Resolve(ctx, set, rend, "app_2", ""); got.Lang != "en" {
			t.Fatalf("uninstalled language: %+v", got)
		}
		// The dashboard asks at the org level.
		if got := look.Dashboard(ctx, set); got != "minimal-dark" {
			t.Fatalf("dashboard theme: %q", got)
		}
	})
}
