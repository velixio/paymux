package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Audit (2026-10, authz lane). F52 says role changes and provider accounts need
// a recent full sign-in (password + code within 12 hours). These tests drive a
// stale-but-live admin session (signed in 13 hours ago, used 2 hours ago) and
// expect every such route to send it to /dashboard/reauth. They fail on the
// current code: the role routes and POST /providers/{id}/apps run without the
// d.recent guard.

func staleAdmin(t *testing.T, e *env) *http.Client {
	t.Helper()
	_, secret := e.seedUser("root@example.com", "admin", true)
	c := e.client()
	e.loginAs(c, "root@example.com", secret, true)
	e.clk.Add(11 * time.Hour)
	e.pageOK(c, "/dashboard/overview") // keep it alive (idle limit)
	e.clk.Add(2 * time.Hour)           // 13 hours since the full sign-in
	if !e.live(c) {
		t.Fatal("session should still be live")
	}
	return c
}

func wantReauth(t *testing.T, what string, r *http.Response) {
	t.Helper()
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/dashboard/reauth") {
		t.Errorf("%s with a 13-hour-old sign-in: %d %q, want a redirect to /dashboard/reauth", what, r.StatusCode, r.Header.Get("Location"))
	}
}

func TestAuditAuthzRoleChangesNeedRecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := staleAdmin(t, e)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/roles"))

		r, _ := e.post(c, "/dashboard/roles", url.Values{"csrf": {tok}, "name": {"payer"},
			"action": {"payout.create", "api_key.create", "payment.read"}})
		wantReauth(t, "POST /dashboard/roles (create a role with payout.create + api_key.create)", r)

		// Seed a custom role directly so update/delete can be exercised too.
		if err := db.CreateRole(ctx, "clerk", []string{"payment.read"}, e.clk.Now()); err != nil && !strings.Contains(err.Error(), "exists") {
			t.Fatal(err)
		}
		e.d.roles.Invalidate()
		r, _ = e.post(c, "/dashboard/roles/clerk", url.Values{"csrf": {tok}, "action": {"payout.create", "api_key.create"}})
		wantReauth(t, "POST /dashboard/roles/{name} (grant payout.create)", r)
	})
}

func TestAuditAuthzProviderAppLinkNeedsRecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		st := fakeconn.RegisterSchema("auditfake", providerSchema)
		pa := e.createProvider(st, `{"key_id":"k","key_secret":"s"}`)
		app := e.seedApp("alpha")
		c := staleAdmin(t, e)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/providers/"+pa.ID))

		// The same change through the app page asks for a fresh sign-in ...
		r, _ := e.post(c, "/dashboard/apps/"+app.ID+"/providers", url.Values{"csrf": {tok}, "account": {pa.ID}})
		wantReauth(t, "POST /dashboard/apps/{id}/providers", r)
		// ... but not through the provider page.
		r, _ = e.post(c, "/dashboard/providers/"+pa.ID+"/apps", url.Values{"csrf": {tok}, "app_id": {app.ID}})
		wantReauth(t, "POST /dashboard/providers/{id}/apps", r)
	})
}

// F52 also lists "users" and "keys". Disabling another admin, signing a user
// out everywhere and revoking an API key run without the guard, so a stale
// remembered cookie can lock the other admins out or kill the production key.
func TestAuditAuthzUserAndKeyRevocationNeedRecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		other, _ := e.seedUser("second-admin@example.com", "admin", true)
		app := e.seedApp("alpha")
		_, key, err := e.core.CreateAPIKey(ctx, auditUser("usr_seed"), orgScope(), app.ID, "live")
		if err != nil {
			t.Fatal(err)
		}
		c := staleAdmin(t, e)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/users"))

		r, _ := e.post(c, "/dashboard/users/"+other+"/disable", url.Values{"csrf": {tok}})
		wantReauth(t, "POST /dashboard/users/{id}/disable (another admin)", r)
		r, _ = e.post(c, "/dashboard/users/"+other+"/sessions/revoke", url.Values{"csrf": {tok}})
		wantReauth(t, "POST /dashboard/users/{id}/sessions/revoke", r)
		r, _ = e.post(c, "/dashboard/apps/"+app.ID+"/keys/"+key.ID+"/revoke", url.Values{"csrf": {tok}})
		wantReauth(t, "POST /dashboard/apps/{id}/keys/{kid}/revoke (live key)", r)
	})
}
