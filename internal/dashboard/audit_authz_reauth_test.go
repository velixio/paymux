package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
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

// R-A9: the remaining configuration writes also need a recent full sign-in.
// A stale remembered cookie must not add a webhook endpoint (an attacker's URL
// then receives every event), add a domain, or change org, app or appearance
// settings. The routes that exist under these names are the endpoint add and
// disable, the domain add, the org settings (one key and the routing editor),
// the app settings (set and reset) and the two appearance saves.
func reauthConfigRoutes(app store.App, sinkURL string) []struct {
	what, path string
	form       url.Values
	loose      bool // a fresh sign-in must pass the guard; the stub form need not be valid
} {
	a := "/dashboard/apps/" + app.ID
	return []struct {
		what, path string
		form       url.Values
		loose      bool
	}{
		{"endpoint add", a + "/endpoints", url.Values{"url": {sinkURL}, "events": {"payment.succeeded"}}, false},
		{"endpoint disable", a + "/endpoints/whe_none/disable", url.Values{}, true},
		{"domain add", a + "/domains", url.Values{"host": {"pay.alpha.example"}}, false},
		{"org setting", "/dashboard/settings/payment_ttl", url.Values{"value": {"45"}}, false},
		{"org routing", "/dashboard/settings/routing", routingForm(), false},
		{"app setting", a + "/settings/payment_ttl", url.Values{"value": {"45"}}, false},
		{"app setting reset", a + "/settings/payment_ttl/reset", url.Values{}, false},
		{"branding", a + "/branding", url.Values{"display_name": {"Alpha"}}, true},
		{"app tax", a + "/tax", url.Values{}, true},
		{"fx", "/dashboard/fx", url.Values{}, true},
		{"tax entity create", "/dashboard/tax-entities", url.Values{}, true},
		{"tax entity update", "/dashboard/tax-entities/ten_none", url.Values{}, true},
		{"tax entity dsc set", "/dashboard/tax-entities/ten_none/dsc", url.Values{}, true},
		{"tax entity dsc remove", "/dashboard/tax-entities/ten_none/dsc/remove", url.Values{}, true},
		{"theme", "/dashboard/appearance/theme", url.Values{"theme": {"pink"}}, false},
		{"app appearance", "/dashboard/appearance/apps/" + app.ID, url.Values{"theme": {"pink"}, "language": {""}}, false},
	}
}

func TestAuditAuthzConfigWritesNeedRecentSignIn(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer sink.Close()
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("pink", pinkTheme, nil)
		app := e.seedApp("alpha")
		c := staleAdmin(t, e.env)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))
		for _, rt := range reauthConfigRoutes(app, sink.URL) {
			f := url.Values{"csrf": {tok}}
			for k, v := range rt.form {
				f[k] = v
			}
			r, _ := e.post(c, rt.path, f)
			wantReauth(t, "POST "+rt.path+" ("+rt.what+")", r)
		}
	})
}

// The guard only asks for a recent sign-in: with one, every route still works.
func TestAuditAuthzConfigWritesWorkWithRecentSignIn(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer sink.Close()
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newThemedEnv(t, db)
		e.writeTheme("pink", pinkTheme, nil)
		app := e.seedApp("alpha")
		c := e.roleClient("admin")
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))
		for _, rt := range reauthConfigRoutes(app, sink.URL) {
			f := url.Values{"csrf": {tok}}
			for k, v := range rt.form {
				f[k] = v
			}
			r, body := e.post(c, rt.path, f)
			if strings.HasPrefix(r.Header.Get("Location"), "/dashboard/reauth") || r.StatusCode >= 400 && !rt.loose {
				t.Errorf("POST %s (%s) with a fresh sign-in: %d %q\n%.300s", rt.path, rt.what, r.StatusCode, r.Header.Get("Location"), body)
			}
		}
	})
}

// routingForm is a valid save of the routing editor, without the CSRF token.
func routingForm() url.Values {
	f := routeForm("", []map[string]string{{"r_name": "inr", "r_currency": "INR", "r_action": "route", "r_targets": "razorpay"}},
		map[string]string{"ca_targets": "razorpay, whop"})
	f.Del("csrf")
	return f
}
