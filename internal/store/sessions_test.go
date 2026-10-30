package store_test

import (
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestSessionsKeepRememberAuthenticationTimeAndHintAndCanBeListedAndRevoked(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		for _, id := range []string{"usr_a", "usr_b"} {
			if err := db.InsertUser(ctx, store.UserRow{ID: id, Email: id + "@example.com", PasswordHash: "h", TOTPLastStep: -1, Role: "viewer", CreatedAt: t0}); err != nil {
				t.Fatal(err)
			}
		}
		mk := func(id, user, tok string, remember bool, auth time.Time) store.SessionRow {
			return store.SessionRow{ID: id, UserID: user, TokenHash: tok, CSRFKey: []byte{1}, CreatedAt: t0, LastSeenAt: t0.Add(time.Minute),
				ExpiresAt: t0.Add(30 * 24 * time.Hour), IP: "203.0.113.9", UserAgent: "UA", Remember: remember, AuthenticatedAt: auth, IPHint: "203.0.113.0/24"}
		}
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(db.InsertSession(ctx, mk("ses_1", "usr_a", "h1", true, t0.Add(5*time.Second))))
		must(db.InsertSession(ctx, mk("ses_2", "usr_a", "h2", false, time.Time{})))
		must(db.InsertSession(ctx, mk("ses_3", "usr_b", "h3", false, t0)))

		s1, _, err := db.SessionByTokenHash(ctx, "h1")
		must(err)
		if !s1.Remember || !s1.AuthenticatedAt.Equal(t0.Add(5*time.Second)) || s1.IPHint != "203.0.113.0/24" {
			t.Errorf("session 1: %+v", s1)
		}
		// A session without an authentication time (made before the column) counts as authenticated when it began.
		s2, _, _ := db.SessionByTokenHash(ctx, "h2")
		if s2.Remember || !s2.AuthenticatedAt.Equal(s2.CreatedAt) {
			t.Errorf("session 2: %+v", s2)
		}

		list, err := db.ListUserSessions(ctx, "usr_a")
		if err != nil || len(list) != 2 {
			t.Fatalf("list: %v %v", list, err)
		}
		// Revoking is scoped to the user: usr_b cannot revoke usr_a's session.
		if ok, err := db.DeleteUserSession(ctx, "usr_b", "ses_1"); err != nil || ok {
			t.Errorf("another user's session was revoked: %v %v", ok, err)
		}
		if ok, err := db.DeleteUserSession(ctx, "usr_a", "ses_1"); err != nil || !ok {
			t.Errorf("own session: %v %v", ok, err)
		}
		must(db.InsertSession(ctx, mk("ses_4", "usr_a", "h4", true, t0)))
		n, err := db.DeleteUserSessionsExcept(ctx, "usr_a", "ses_4")
		if err != nil || n != 1 {
			t.Errorf("sign out the others: %d %v", n, err)
		}
		if list, _ := db.ListUserSessions(ctx, "usr_a"); len(list) != 1 || list[0].ID != "ses_4" {
			t.Errorf("left: %+v", list)
		}
		if list, _ := db.ListUserSessions(ctx, "usr_b"); len(list) != 1 {
			t.Errorf("another user's sessions were touched: %+v", list)
		}
		must(db.SetSessionAuthenticated(ctx, "ses_4", t0.Add(time.Hour)))
		s4, _, _ := db.SessionByTokenHash(ctx, "h4")
		if !s4.AuthenticatedAt.Equal(t0.Add(time.Hour)) {
			t.Errorf("authenticated_at %v", s4.AuthenticatedAt)
		}
	})
}
