package dashboard

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func auditCount(t *testing.T, db *store.DB, action string) int {
	t.Helper()
	var n int
	_ = db.Read(ctx, func(q store.Querier) error {
		rows, err := store.ListAudit(ctx, q, store.DefaultOrgID, action, 50)
		n = len(rows)
		return err
	})
	return n
}

func TestAdminCreatesAndAssignsCustomRoles(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		_, secret := e.seedUser("root@example.com", "admin", true)
		e.login(e.c, "root@example.com", secret)
		tok := e.authedCSRF(e.c)

		// The built-in roles are listed (seeded by bootstrap) and cannot be touched.
		_, body := e.get(e.c, "/dashboard/roles")
		for _, want := range []string{"admin", "operator", "viewer", "built-in", "New role"} {
			if !strings.Contains(body, want) {
				t.Errorf("roles page lacks %q", want)
			}
		}
		if strings.Contains(body, `value="user.manage"`) || strings.Contains(body, `value="role.manage"`) {
			t.Error("admin-only actions are offered for custom roles")
		}

		bad := map[string]url.Values{
			"name":        {"name": {"Bad Name"}, "action": {"payment.read"}},
			"built-in":    {"name": {"viewer"}, "action": {"payment.read"}},
			"ungrantable": {"name": {"sneaky"}, "action": {"user.manage"}},
			"unknown":     {"name": {"sneaky"}, "action": {"payment.explode"}},
		}
		for name, form := range bad {
			form.Set("csrf", tok)
			if r, _ := e.post(e.c, "/dashboard/roles", form); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if rows, _ := db.ListRoles(ctx); len(rows) != 3 {
			t.Errorf("a rejected role was stored: %+v", rows)
		}

		r, _ := e.post(e.c, "/dashboard/roles", url.Values{"name": {"auditor"}, "action": {"payment.read", "audit.read", "payment.read"}, "csrf": {tok}})
		if r.StatusCode != 303 {
			t.Fatalf("create: %d", r.StatusCode)
		}
		if r, _ := e.post(e.c, "/dashboard/roles", url.Values{"name": {"auditor"}, "action": {"payment.read"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("duplicate: %d", r.StatusCode)
		}
		if auditCount(t, db, "role.create") != 1 {
			t.Error("role.create not audited")
		}

		// Assign it to a user; the role shows in the users page.
		uid, usecret := e.seedUser("aud@example.com", "viewer", true)
		if r, _ := e.post(e.c, "/dashboard/users/"+uid+"/role", url.Values{"role": {"auditor"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("assign: %d", r.StatusCode)
		}
		if _, body := e.get(e.c, "/dashboard/users"); !strings.Contains(body, `<option value="auditor"`) {
			t.Error("the users page does not offer the custom role")
		}
		if r, _ := e.post(e.c, "/dashboard/users/"+uid+"/role", url.Values{"role": {"nonesuch"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("assign unknown role: %d", r.StatusCode)
		}
		if r, _ := e.post(e.c, "/dashboard/users", url.Values{"email": {"made@example.com"}, "role": {"auditor"}, "password": {goodPW}, "csrf": {tok}}); r.StatusCode != 200 {
			t.Errorf("create a user with a custom role: %d", r.StatusCode)
		}

		// The role works: audit.read and payment.read, nothing else.
		c := e.client()
		e.login(c, "aud@example.com", usecret)
		if r, _ := e.get(c, "/dashboard/payments"); r.StatusCode != 200 {
			t.Errorf("payments with payment.read: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/audit"); r.StatusCode != 200 {
			t.Errorf("audit with audit.read: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/users"); r.StatusCode != 403 { // settings.read not granted
			t.Errorf("users without settings.read: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/roles"); r.StatusCode != 403 {
			t.Errorf("a custom role reached the roles page: %d", r.StatusCode)
		}
		ctok := csrfOfPage(t, e, c)
		if r, _ := e.post(c, "/dashboard/roles", url.Values{"name": {"self"}, "action": {"payment.read"}, "csrf": {ctok}}); r.StatusCode != 403 {
			t.Errorf("a custom role created a role: %d", r.StatusCode)
		}

		// Editing takes effect immediately in this process (the cache is invalidated).
		if r, _ := e.post(e.c, "/dashboard/roles/auditor", url.Values{"action": {"payment.read", "audit.read", "settings.read"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("update: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/users"); r.StatusCode != 200 {
			t.Errorf("users after granting settings.read: %d", r.StatusCode)
		}
		if r, _ := e.post(e.c, "/dashboard/roles/auditor", url.Values{"action": {"payment.read"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("update 2: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/audit"); r.StatusCode != 403 {
			t.Errorf("audit after revoking audit.read: %d", r.StatusCode)
		}
		if auditCount(t, db, "role.update") != 2 {
			t.Error("role.update not audited")
		}

		// Delete: refused while assigned, built-ins never.
		if r, _ := e.post(e.c, "/dashboard/roles/auditor/delete", url.Values{"csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("delete in use: %d", r.StatusCode)
		}
		for _, n := range []string{"viewer", "admin"} {
			if r, _ := e.post(e.c, "/dashboard/roles/"+n+"/delete", url.Values{"csrf": {tok}}); r.StatusCode != 400 {
				t.Errorf("delete built-in %s: %d", n, r.StatusCode)
			}
			if r, _ := e.post(e.c, "/dashboard/roles/"+n, url.Values{"action": {"payment.read"}, "csrf": {tok}}); r.StatusCode != 400 {
				t.Errorf("edit built-in %s: %d", n, r.StatusCode)
			}
		}
		if r, _ := e.post(e.c, "/dashboard/roles/nonesuch", url.Values{"action": {"payment.read"}, "csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("edit missing: %d", r.StatusCode)
		}
		made, err := db.UserByEmail(ctx, "made@example.com")
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range []string{uid, made.ID} {
			if r, _ := e.post(e.c, "/dashboard/users/"+u+"/role", url.Values{"role": {"viewer"}, "csrf": {tok}}); r.StatusCode != 303 {
				t.Fatalf("reassign %s: %d", u, r.StatusCode)
			}
		}
		if r, _ := e.post(e.c, "/dashboard/roles/auditor/delete", url.Values{"csrf": {tok}}); r.StatusCode != 303 {
			t.Errorf("delete: %d", r.StatusCode)
		}
		if auditCount(t, db, "role.delete") != 1 {
			t.Error("role.delete not audited")
		}
		// A deleted role grants nothing to a user who still held it in a stale session.
		if ok, _ := db.RoleExists(ctx, "auditor"); ok {
			t.Error("role still exists")
		}
	})
}

func TestNonAdminsCannotManageRoles(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.seedUser("root@example.com", "admin", true)
		for _, role := range []string{"viewer", "operator"} {
			email := role + "@example.com"
			_, secret := e.seedUser(email, role, true)
			c := e.client()
			e.login(c, email, secret)
			tok := csrfOfPage(t, e, c)
			if r, _ := e.get(c, "/dashboard/roles"); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s: roles page %d", role, r.StatusCode)
			}
			if r, _ := e.post(c, "/dashboard/roles", url.Values{"name": {"mine"}, "action": {"payment.read"}, "csrf": {tok}}); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s: create %d", role, r.StatusCode)
			}
		}
		if rows, _ := db.ListRoles(ctx); len(rows) != 3 {
			t.Errorf("roles = %+v", rows)
		}
	})
}
