package dashboard

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func (s *sso) signIn(email, sub string) *http.Response {
	s.t.Helper()
	c := s.client()
	resp, _ := s.get(c, s.begin(c, map[string]any{"sub": sub, "email": email, "email_verified": true}))
	return resp
}

func (s *sso) allowLink(admin *http.Client, userID string) int {
	s.t.Helper()
	tok := csrfOf(s.t, s.pageOK(admin, "/dashboard/users"))
	r, _ := s.post(admin, "/dashboard/users/"+userID+"/sso-link", url.Values{"csrf": {tok}})
	return r.StatusCode
}

// A verified e-mail alone does not hand over an account that has a password and
// two-factor sign-in: an admin has to say the person may use single sign-on.
func TestSSORefusesAPasswordAccountUnlessAnAdminLinkedIt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		id, _ := s.seedUser("alice@example.com", "operator", true)
		if r := s.signIn("alice@example.com", "sub-alice"); r.StatusCode == 303 && sessionCookie(r) != nil {
			t.Fatal("an unlinked password account was signed in by e-mail alone")
		}
		admin := s.roleClient("admin")
		if code := s.allowLink(admin, id); code != 303 {
			t.Fatalf("link: %d", code)
		}
		if r := s.signIn("alice@example.com", "sub-alice"); r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Fatalf("after the admin linked it: %d", r.StatusCode)
		}
		if auditCount(t, db, "user.sso_link_allow") != 1 || auditCount(t, db, "user.sso_link") != 1 {
			t.Error("the link and its use are not audited")
		}
		// Only admins link accounts.
		for _, role := range []string{"viewer", "operator"} {
			c := s.roleClient(role)
			tok := csrfOf(t, s.pageOK(c, "/dashboard/payments"))
			if r, _ := s.post(c, "/dashboard/users/"+id+"/sso-link", url.Values{"csrf": {tok}}); r.StatusCode != 403 {
				t.Errorf("%s may link accounts: %d", role, r.StatusCode)
			}
		}
	})
}

// After the first sign-in the account is tied to the identity provider's
// (issuer, subject), not to an e-mail address.
func TestSSOMatchesOnIssuerAndSubjectAfterTheFirstSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		id, _ := s.seedUser("alice@example.com", "operator", true)
		admin := s.roleClient("admin")
		s.allowLink(admin, id)
		if r := s.signIn("alice@example.com", "sub-alice"); r.StatusCode != 303 {
			t.Fatalf("first: %d", r.StatusCode)
		}
		// Somebody else at the identity provider who comes to hold the same address.
		if r := s.signIn("alice@example.com", "sub-someone-else"); r.StatusCode == 303 && sessionCookie(r) != nil {
			t.Error("a different subject with the same e-mail was signed in as alice")
		}
		// The same subject, whatever address the provider now reports (in an allowed domain).
		r := s.signIn("alice.renamed@example.com", "sub-alice")
		if r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Fatalf("same subject, new address: %d", r.StatusCode)
		}
		if u, _ := db.UserByID(ctx, id); u.Email != "alice@example.com" || u.Role != "operator" {
			t.Errorf("the account changed: %+v", u)
		}
		if n, _ := db.CountUsers(ctx); n != 2 { // alice and the admin: no user was created
			t.Errorf("users = %d", n)
		}
	})
}

// The allowed domains apply to every single sign-in, not only to creation.
func TestSSOAppliesAllowedDomainsOnEverySignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		id, _ := s.seedUser("alice@example.com", "operator", true)
		s.allowLink(s.roleClient("admin"), id)
		if r := s.signIn("alice@example.com", "sub-alice"); r.StatusCode != 303 {
			t.Fatalf("first: %d", r.StatusCode)
		}
		// The account at the provider moves to a domain nobody here controls.
		if r := s.signIn("alice@evil.example.org", "sub-alice"); r.StatusCode == 303 && sessionCookie(r) != nil {
			t.Error("a linked identity signed in with an address outside the allowed domains")
		}
	})
}

// Users that single sign-on created before identities were recorded (no
// password, no identity) are bound on their next sign-in.
func TestSSOCreatedUsersAreBoundAtTheirNextSignIn(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		legacy := store.UserRow{ID: "usr_legacy", Email: "new.hire@example.com", PasswordHash: ssoPasswordHash, TOTPLastStep: -1, Role: "viewer", CreatedAt: time.Now()}
		if err := db.InsertUser(ctx, legacy); err != nil {
			t.Fatal(err)
		}
		if r := s.signIn("new.hire@example.com", "sub-new"); r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Fatalf("legacy user: %d", r.StatusCode)
		}
		if r := s.signIn("new.hire@example.com", "sub-impostor"); r.StatusCode == 303 && sessionCookie(r) != nil {
			t.Error("another subject took over the bound account")
		}
	})
}

// A user created by single sign-on is bound from the start.
func TestSSOCreatedUserIsBoundAtCreation(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		s := newSSO(t, db, nil)
		if r := s.signIn("new.hire@example.com", "sub-new"); r.StatusCode != 303 {
			t.Fatalf("provisioning: %d", r.StatusCode)
		}
		if r := s.signIn("new.hire@example.com", "sub-impostor"); r.StatusCode == 303 && sessionCookie(r) != nil {
			t.Error("another subject took over the created account")
		}
		if r := s.signIn("new.hire@example.com", "sub-new"); r.StatusCode != 303 || sessionCookie(r) == nil {
			t.Errorf("the owner is refused: %d", r.StatusCode)
		}
	})
}
