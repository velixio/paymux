package dashboard

import (
	"net/url"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Audit H-3: session lifetimes and einvoice_irp_keys are admin settings. A
// custom role holding settings.write gets 403 on them and can still change
// ordinary settings; an admin needs a recent sign-in.
func TestAuditH3AdminOnlySettings(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		if err := db.CreateRole(ctx, "clerk", []string{"settings.read", "settings.write"}, e.clk.Now()); err != nil {
			t.Fatal(err)
		}
		e.d.roles.Invalidate()
		_, secret := e.seedUser("clerk@example.com", "clerk", true)
		c := e.client()
		e.login(c, "clerk@example.com", secret)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))

		for key, val := range map[string]string{"session_idle_hours": "720", "session_max_days": "90", "session_remember_days": "90",
			"einvoice_irp_keys": "[]"} {
			if r, _ := e.post(c, "/dashboard/settings/"+key, url.Values{"csrf": {tok}, "value": {val}}); r.StatusCode != 403 {
				t.Errorf("custom role setting %s: %d, want 403", key, r.StatusCode)
			}
		}
		if r, _ := e.post(c, "/dashboard/settings/payment_ttl", url.Values{"csrf": {tok}, "value": {"45"}}); r.StatusCode != 303 {
			t.Errorf("custom role setting payment_ttl: %d, want 303", r.StatusCode)
		}
	})
}

func TestAuditH3AdminSettingsNeedRecentSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		c := staleAdmin(t, e)
		tok := csrfOf(t, e.pageOK(c, "/dashboard/settings"))
		for _, key := range []string{"session_idle_hours", "session_max_days", "session_remember_days", "einvoice_irp_keys"} {
			r, _ := e.post(c, "/dashboard/settings/"+key, url.Values{"csrf": {tok}, "value": {"1"}})
			wantReauth(t, "POST /dashboard/settings/"+key, r)
		}
	})
}
