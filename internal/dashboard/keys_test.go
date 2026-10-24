package dashboard

import (
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestScopedAndRotatedKeysFromTheDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		tok := csrfOf(t, e.pageOK(admin, page))

		// The form offers the scopes; ticking some limits the key.
		body := e.pageOK(admin, page+"?tab=keys")
		has(t, body, `name="scope" value="payments:read"`, `name="scope" value="invoices:read"`, "Rotate")
		r, body := e.post(admin, page+"/keys", url.Values{"mode": {"live"}, "scope": {"payments:read", "refunds:write"}, "csrf": {tok}})
		if r.StatusCode != 200 {
			t.Fatalf("create: %d", r.StatusCode)
		}
		old := keyTokenRE.FindString(body)
		pr, err := e.core.Authenticate(ctx, old)
		if err != nil || !reflect.DeepEqual(pr.Scopes, []string{"payments:read", "refunds:write"}) {
			t.Fatalf("scoped key: %+v %v", pr, err)
		}
		if r, _ := e.post(admin, page+"/keys", url.Values{"mode": {"live"}, "scope": {"payments:delete"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("unknown scope: %d", r.StatusCode)
		}
		listing := e.pageOK(admin, page+"?tab=keys")
		has(t, listing, "payments:read, refunds:write")

		// Rotate: the new key keeps the scopes, the old one gets an expiry.
		r, body = e.post(admin, page+"/keys/rotate", url.Values{"mode": {"live"}, "grace_hours": {"6"}, "csrf": {tok}})
		if r.StatusCode != 200 {
			t.Fatalf("rotate: %d\n%s", r.StatusCode, body)
		}
		newTok := keyTokenRE.FindString(body)
		if newTok == "" || newTok == old {
			t.Fatalf("no new key shown: %s", body)
		}
		has(t, body, "1 previous key(s) stop working in 6 h")
		for name, tkn := range map[string]string{"old": old, "new": newTok} {
			if _, err := e.core.Authenticate(ctx, tkn); err != nil {
				t.Errorf("%s key during the grace period: %v", name, err)
			}
		}
		if p, _ := e.core.Authenticate(ctx, newTok); !reflect.DeepEqual(p.Scopes, []string{"payments:read", "refunds:write"}) {
			t.Errorf("rotated key scopes %v", p.Scopes)
		}
		var keys []store.APIKey
		_ = db.Read(ctx, func(q store.Querier) (err error) { keys, err = store.ListAPIKeys(ctx, q, app.ID); return })
		if len(keys) != 2 || !keys[0].ExpiresAt.Equal(e.clk.Now().Add(6*time.Hour)) || !keys[1].ExpiresAt.IsZero() {
			t.Fatalf("keys after rotation: %+v", keys)
		}
		has(t, e.pageOK(admin, page+"?tab=keys"), "Expires")

		for name, form := range map[string]url.Values{
			"zero hours": {"mode": {"live"}, "grace_hours": {"0"}}, "text": {"mode": {"live"}, "grace_hours": {"soon"}},
			"bad mode": {"mode": {"staging"}}, "bad scope": {"mode": {"live"}, "scope": {"x"}},
		} {
			form.Set("csrf", tok)
			if r, _ := e.post(admin, page+"/keys/rotate", form); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		// Only the key managers may rotate.
		operator := e.roleClient("operator")
		ot := csrfOf(t, e.pageOK(operator, page))
		if r, _ := e.post(operator, page+"/keys/rotate", url.Values{"mode": {"live"}, "csrf": {ot}}); r.StatusCode != 403 {
			t.Errorf("operator rotate: %d", r.StatusCode)
		}
		if n := auditCount(t, db, "api_key.rotate"); n != 1 {
			t.Errorf("rotation audited %d times", n)
		}
		if !strings.Contains(auditAfter(t, db, "api_key.rotate"), "payments:read,refunds:write") {
			t.Error("the rotated key's scopes are not in the audit entry")
		}
	})
}

func auditAfter(t *testing.T, db *store.DB, action string) string {
	t.Helper()
	var out string
	_ = db.Read(ctx, func(q store.Querier) error {
		rows, err := store.ListAudit(ctx, q, store.DefaultOrgID, action, 5)
		for _, r := range rows {
			out += r.After
		}
		return err
	})
	return out
}

func TestKeyRevealMarkedOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		admin := e.roleClient("admin")
		page := "/dashboard/apps/" + app.ID
		tok := csrfOf(t, e.pageOK(admin, page))
		if strings.Contains(e.pageOK(admin, page+"?tab=keys"), "data-once") {
			t.Fatal("the plain keys page is not a one-time view")
		}
		r, body := e.post(admin, page+"/keys", url.Values{"mode": {"live"}, "csrf": {tok}})
		if r.StatusCode != 200 {
			t.Fatalf("create: %d", r.StatusCode)
		}
		has(t, body, `data-once`)
		if keyTokenRE.FindString(body) == "" {
			t.Fatal("no key revealed")
		}
	})
}

// The other one-time secret views carry the marker in their templates.
func TestOneTimeTemplatesMarkedOnce(t *testing.T) {
	for _, f := range []string{"dash_app.html", "dash_setup_done.html", "dash_subscription.html", "dash_users.html"} {
		b, err := os.ReadFile("../../web/templates/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "data-once") {
			t.Errorf("%s: missing data-once", f)
		}
	}
}
