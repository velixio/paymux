package settings_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestThemeAndCustomerLanguageSettings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s, app := setup(t, db)
		org := store.DefaultOrgID
		for key, def := range map[string]string{"theme": `"default"`, "customer_language": `"en"`} {
			if v := resolve(t, s, app, key); string(v.Raw) != def || v.Source != settings.SourceDefault {
				t.Fatalf("%s default: %+v", key, v)
			}
		}
		for _, c := range []struct{ key, val string }{
			{"theme", `"minimal-dark"`}, {"customer_language", `"hi"`},
		} {
			if err := s.SetOrg(ctx, admin, org, c.key, json.RawMessage(c.val)); err != nil {
				t.Fatalf("SetOrg %s: %v", c.key, err)
			}
			if err := s.SetApp(ctx, admin, org, app, c.key, json.RawMessage(c.val)); err != nil {
				t.Fatalf("SetApp %s: %v", c.key, err)
			}
		}
		for _, c := range []struct{ key, val string }{
			{"theme", `"Bad Name"`}, {"theme", `"../etc"`}, {"theme", `""`}, {"theme", `7`},
			{"customer_language", `"english"`}, {"customer_language", `"EN"`}, {"customer_language", `""`}, {"customer_language", `"../hi"`},
		} {
			if err := s.SetOrg(ctx, admin, org, c.key, json.RawMessage(c.val)); !errors.Is(err, settings.ErrInvalid) {
				t.Errorf("%s=%s accepted (err %v)", c.key, c.val, err)
			}
		}
	})
}
