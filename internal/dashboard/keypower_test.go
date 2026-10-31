package dashboard

import (
	"net/url"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// api_key.create is not a back door to payout and refund power: a key may only
// carry what the person issuing it could do themselves.
func TestAPIKeyCreateCannotHandOutPowerTheRoleLacks(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("root@example.com", "admin", true)
		e.login(e.c, "root@example.com", secret)
		tok := e.authedCSRF(e.c)
		if r, _ := e.post(e.c, "/dashboard/roles", url.Values{"name": {"keyer"}, "action": {"api_key.create", "payment.read"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("role: %d", r.StatusCode)
		}
		uid, usecret := e.seedUser("keyer@example.com", "viewer", true)
		if r, _ := e.post(e.c, "/dashboard/users/"+uid+"/role", url.Values{"role": {"keyer"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("assign: %d", r.StatusCode)
		}
		app := e.seedApp("alpha")
		page := "/dashboard/apps/" + app.ID
		// The admin's full-access key exists already (to be rotated later).
		if r, _ := e.post(e.c, page+"/keys", url.Values{"mode": {"live"}, "csrf": {tok}}); r.StatusCode != 200 {
			t.Fatalf("admin key: %d", r.StatusCode)
		}
		c := e.client()
		e.login(c, "keyer@example.com", usecret)
		ktok := csrfOfPage(t, e, c)
		keys := func() int {
			var ks []store.APIKey
			_ = db.Read(ctx, func(q store.Querier) (err error) { ks, err = store.ListAPIKeys(ctx, q, app.ID); return })
			return len(ks)
		}
		before := keys()
		for name, form := range map[string]url.Values{
			"payout scope":       {"mode": {"live"}, "scope": {"payouts:write"}},
			"payout read scope":  {"mode": {"live"}, "scope": {"payouts:read"}},
			"refund scope":       {"mode": {"live"}, "scope": {"refunds:write"}},
			"payment write":      {"mode": {"live"}, "scope": {"payments:write"}},
			"full access (none)": {"mode": {"live"}},
		} {
			form.Set("csrf", ktok)
			if r, _ := e.post(c, page+"/keys", form); r.StatusCode != 403 {
				t.Errorf("%s: %d, want 403", name, r.StatusCode)
			}
		}
		if keys() != before {
			t.Errorf("a refused request created a key")
		}
		// Rotating the full-access key into a new full-access key is the same power.
		if r, _ := e.post(c, page+"/keys/rotate", url.Values{"mode": {"live"}, "csrf": {ktok}}); r.StatusCode != 403 {
			t.Errorf("rotate inheriting full access: %d", r.StatusCode)
		}
		if keys() != before {
			t.Errorf("a refused rotation created a key")
		}
		// What the role can do itself is allowed.
		if r, _ := e.post(c, page+"/keys", url.Values{"mode": {"live"}, "scope": {"payments:read"}, "csrf": {ktok}}); r.StatusCode != 200 {
			t.Errorf("a read-only key: %d", r.StatusCode)
		}
		if r, _ := e.post(c, page+"/keys/rotate", url.Values{"mode": {"live"}, "scope": {"payments:read"}, "csrf": {ktok}}); r.StatusCode != 200 {
			t.Errorf("rotating into a read-only key: %d", r.StatusCode)
		}
		// An admin may do all of it, payouts included.
		for _, sc := range []string{"payouts:write", "refunds:write"} {
			if r, _ := e.post(e.c, page+"/keys", url.Values{"mode": {"live"}, "scope": {sc}, "csrf": {tok}}); r.StatusCode != 200 {
				t.Errorf("admin %s: %d", sc, r.StatusCode)
			}
		}
	})
}
