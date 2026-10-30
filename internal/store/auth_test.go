package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestUsersCRUD(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		now := time.UnixMilli(1_800_000_000_000).UTC()
		u := store.UserRow{ID: "usr_1", Email: "a@example.com", PasswordHash: "h", TOTPLastStep: -1, Role: "admin", CreatedAt: now}
		if err := db.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertUser(ctx, store.UserRow{ID: "usr_2", Email: "a@example.com", PasswordHash: "h", TOTPLastStep: -1, Role: "viewer", CreatedAt: now}); !errors.Is(err, store.ErrDuplicate) {
			t.Fatalf("duplicate email: %v", err)
		}
		if err := db.InsertUser(ctx, store.UserRow{ID: "usr_3", Email: "b@example.com", PasswordHash: "h", TOTPLastStep: -1, Role: "root", CreatedAt: now}); err == nil {
			t.Fatal("bad role accepted")
		}
		got, err := db.UserByEmail(ctx, "a@example.com")
		if err != nil || got.ID != "usr_1" || got.TOTPSecretEnc != nil || got.TOTPLastStep != -1 || got.Disabled || !got.CreatedAt.Equal(now) {
			t.Fatalf("by email: %+v %v", got, err)
		}
		if _, err := db.UserByEmail(ctx, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}

		if err := db.SetUserTOTP(ctx, "usr_1", []byte{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
		got, _ = db.UserByID(ctx, "usr_1")
		if string(got.TOTPSecretEnc) != "\x01\x02\x03" {
			t.Fatalf("secret %v", got.TOTPSecretEnc)
		}
		// replay protection: a step is accepted once, and only if newer
		for _, c := range []struct {
			step int64
			want bool
		}{{100, true}, {100, false}, {99, false}, {101, true}} {
			ok, err := db.AdvanceTOTPStep(ctx, "usr_1", c.step)
			if err != nil || ok != c.want {
				t.Fatalf("advance %d: ok=%v err=%v", c.step, ok, err)
			}
		}
		if err := db.ResetUserTOTP(ctx, "usr_1"); err != nil {
			t.Fatal(err)
		}
		got, _ = db.UserByID(ctx, "usr_1")
		if got.TOTPSecretEnc != nil || got.TOTPLastStep != -1 {
			t.Fatalf("after reset: %+v", got)
		}

		if err := db.SetUserRole(ctx, "usr_1", "viewer"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetUserDisabled(ctx, "usr_1", true); err != nil {
			t.Fatal(err)
		}
		got, _ = db.UserByID(ctx, "usr_1")
		if got.Role != "viewer" || !got.Disabled {
			t.Fatalf("%+v", got)
		}
		if err := db.SetUserRole(ctx, "nope", "admin"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("role of missing: %v", err)
		}
		n, _ := db.CountEnabledAdmins(ctx)
		if n != 0 {
			t.Fatalf("admins=%d", n)
		}
		_ = db.SetUserRole(ctx, "usr_1", "admin")
		_ = db.SetUserDisabled(ctx, "usr_1", false)
		if n, _ = db.CountEnabledAdmins(ctx); n != 1 {
			t.Fatalf("admins=%d", n)
		}
		list, err := db.ListUsers(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("list %v %v", list, err)
		}
		if n, _ := db.CountUsers(ctx); n != 1 {
			t.Fatalf("count=%d", n)
		}
	})
}

func TestSessions(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		migrated(t, db)
		now := time.UnixMilli(1_800_000_000_000).UTC()
		_ = db.InsertUser(ctx, store.UserRow{ID: "usr_1", Email: "a@example.com", PasswordHash: "h", TOTPLastStep: -1, Role: "operator", CreatedAt: now})
		s := store.SessionRow{ID: "ses_1", UserID: "usr_1", TokenHash: "th1", CSRFKey: []byte{9, 8, 7}, CreatedAt: now,
			LastSeenAt: now, ExpiresAt: now.Add(24 * time.Hour), IP: "203.0.113.5", UserAgent: "UA"}
		if err := db.InsertSession(ctx, s); err != nil {
			t.Fatal(err)
		}
		got, u, err := db.SessionByTokenHash(ctx, "th1")
		if err != nil || got.ID != "ses_1" || string(got.CSRFKey) != "\x09\x08\x07" || got.IP != "203.0.113.5" || u.Role != "operator" || !got.ExpiresAt.Equal(s.ExpiresAt) {
			t.Fatalf("%+v %+v %v", got, u, err)
		}
		if _, _, err := db.SessionByTokenHash(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		later := now.Add(time.Hour)
		if err := db.TouchSession(ctx, "ses_1", later); err != nil {
			t.Fatal(err)
		}
		got, _, _ = db.SessionByTokenHash(ctx, "th1")
		if !got.LastSeenAt.Equal(later) {
			t.Fatalf("last seen %v", got.LastSeenAt)
		}
		s2 := s
		s2.ID, s2.TokenHash = "ses_2", "th2"
		_ = db.InsertSession(ctx, s2)
		if err := db.DeleteSession(ctx, "ses_1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.SessionByTokenHash(ctx, "th1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("not deleted")
		}
		if err := db.DeleteUserSessions(ctx, "usr_1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.SessionByTokenHash(ctx, "th2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("user sessions not deleted")
		}
		_ = db.InsertSession(ctx, s)
		n, err := db.DeleteExpiredSessions(ctx, s.ExpiresAt.Add(time.Second))
		if err != nil || n != 1 {
			t.Fatalf("expired: %d %v", n, err)
		}
	})
}
