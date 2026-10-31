package core_test

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestBootstrapIsIdempotent(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		for i := 0; i < 3; i++ {
			org, err := core.Bootstrap(ctx, db, settings.New(db))
			if err != nil || org != store.DefaultOrgID {
				t.Fatalf("Bootstrap: %q %v", org, err)
			}
		}
		var orgs, set int
		_ = db.Read(ctx, func(q store.Querier) error {
			_ = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM orgs").Scan(&orgs)
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM org_settings").Scan(&set)
		})
		if orgs != 1 || set != len(settings.Defs()) {
			t.Errorf("orgs=%d settings=%d want 1/%d", orgs, set, len(settings.Defs()))
		}
	})
}

func TestCreateAppAndKeys(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		actor := audit.System
		app, err := e.svc.CreateApp(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "Yupcha", Slug: "yupcha"})
		if err != nil || !strings.HasPrefix(app.ID, "app_") || app.OrgID != store.DefaultOrgID || app.Slug != "yupcha" {
			t.Fatalf("app %+v %v", app, err)
		}
		currencies, err := e.svc.CreateApp(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{
			Name: "Currencies", Slug: "currencies", AcceptedCurrencies: []string{" usd ", "INR", "USD"},
		})
		if err != nil || !slices.Equal(currencies.AcceptedCurrencies, []string{"USD", "INR"}) {
			t.Fatalf("currencies %+v: %v", currencies.AcceptedCurrencies, err)
		}
		if _, err := e.svc.CreateApp(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{
			Name: "Bad currency", Slug: "bad-currency", AcceptedCurrencies: []string{"ZZZ"},
		}); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("unsupported currency: %v", err)
		}
		if _, err := e.svc.CreateApp(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "Dup", Slug: "yupcha"}); !errors.Is(err, core.ErrConflict) {
			t.Errorf("duplicate slug: %v", err)
		}
		for _, bad := range []core.CreateAppInput{{Name: "", Slug: "x"}, {Name: "X", Slug: "Bad Slug"}, {Name: "X", Slug: ""},
			{Name: "X", Slug: "ok", DefaultReturnURL: "ftp://x"}} {
			if _, err := e.svc.CreateApp(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, bad); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%+v: %v", bad, err)
			}
		}

		tok, key, err := e.svc.CreateAPIKey(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, app.ID, "live")
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^pmx_live_sk_[0-9A-Za-z]{43}$`).MatchString(tok) || key.Mode != "live" || key.Hash == tok ||
			!strings.HasPrefix(tok, key.Prefix) || len(key.Prefix) != len("pmx_live_sk_")+8 {
			t.Fatalf("token %q key %+v", tok, key)
		}
		if e.count("SELECT COUNT(*) FROM api_keys WHERE sha256_hash = ?", tok) != 0 {
			t.Error("plaintext key stored")
		}
		if _, _, err := e.svc.CreateAPIKey(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, app.ID, "prod"); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("bad mode: %v", err)
		}
		if _, _, err := e.svc.CreateAPIKey(ctx, actor, core.OrgScope{OrgID: store.DefaultOrgID}, "app_nope", "test"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing app: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action IN ('app.create','api_key.create')") != 3 {
			t.Error("app and key creation must be audited")
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE after LIKE ?", "%"+tok+"%") != 0 {
			t.Error("key leaked into the audit log")
		}

		got, err := e.svc.AppBySlug(ctx, core.OrgScope{OrgID: store.DefaultOrgID}, "yupcha")
		if err != nil || got.ID != app.ID {
			t.Errorf("AppBySlug %+v %v", got, err)
		}
		if _, err := e.svc.AppBySlug(ctx, core.OrgScope{OrgID: store.DefaultOrgID}, "nope"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("AppBySlug missing: %v", err)
		}
	})
}

func TestAuthenticate(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		app, _ := e.svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "A2", Slug: "a2"})
		tok, key, err := e.svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, app.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		pr, err := e.svc.Authenticate(ctx, tok)
		if err != nil || pr.AppID != app.ID || pr.Mode != "test" || pr.KeyID != key.ID || pr.OrgID != store.DefaultOrgID {
			t.Fatalf("principal %+v %v", pr, err)
		}
		if e.count("SELECT COUNT(*) FROM api_keys WHERE id = ? AND last_used_at IS NOT NULL", key.ID) != 1 {
			t.Error("last_used_at not recorded")
		}
		tampered := tok[:len(tok)-1] + string(rune('0'+(tok[len(tok)-1]-'0'+1)%10))
		if tampered == tok {
			tampered = tok[:len(tok)-1] + "Z"
		}
		for name, bad := range map[string]string{
			"empty": "", "garbage": "hello", "tampered": tampered, "wrong mode": strings.Replace(tok, "pmx_test_", "pmx_live_", 1),
			"short": tok[:len(tok)-3], "no prefix": tok[len("pmx_test_sk_"):], "unknown prefix": "pmx_test_sk_" + strings.Repeat("a", 43),
		} {
			if _, err := e.svc.Authenticate(ctx, bad); !errors.Is(err, core.ErrUnauthenticated) {
				t.Errorf("%s: want ErrUnauthenticated, got %v", name, err)
			}
		}
		err = e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE api_keys SET revoked_at = ? WHERE id = ?", time.Now().UnixMilli(), key.ID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Authenticate(ctx, tok); !errors.Is(err, core.ErrUnauthenticated) {
			t.Errorf("revoked key accepted: %v", err)
		}
	})
}
