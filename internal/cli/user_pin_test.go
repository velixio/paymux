package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
)

func TestUserRemovePIN(t *testing.T) {
	lookup := migrateEnv(t.TempDir())
	if code, _, errs := runWithStdin(t, cliPW+"\n", lookup, "user", "create", "--email", "a@example.com", "--role", "viewer", "--password-stdin"); code != 0 {
		t.Fatal(errs)
	}
	ctx := context.Background()
	now := time.Now()
	db := openDB(t, lookup)
	u, _ := db.UserByEmail(ctx, "a@example.com")
	if err := db.SetUserTOTP(ctx, u.ID, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserSigninPIN(ctx, u.ID, []byte{4, 5, 6}, now); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSession(ctx, store.SessionRow{ID: "ses_1", UserID: u.ID, TokenHash: "h", CSRFKey: []byte{1}, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(time.Hour), IP: "x", UserAgent: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx store.Tx) error {
		return store.InsertSigninApproval(ctx, tx, store.SigninApproval{ID: "sia_1", OrgID: "org", UserID: u.ID, BrowserHash: "b", IP: "x",
			Status: store.SigninPending, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	code, out, errs := runWithStdin(t, "", lookup, "user", "remove-pin", "--email", "A@example.com")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	if want := "sign-in PIN removed for a@example.com; their pending approval requests were cancelled.\n"; out != want {
		t.Fatalf("stdout %q, want %q", out, want)
	}
	db = openDB(t, lookup)
	got, _ := db.UserByID(ctx, u.ID)
	if got.SigninPINEnc != nil {
		t.Fatalf("PIN still set: %+v", got)
	}
	if got.TOTPSecretEnc == nil {
		t.Fatal("the authenticator secret was touched")
	}
	if _, _, err := db.SessionByTokenHash(ctx, "h"); err != nil {
		t.Fatalf("the session was touched: %v", err)
	}
	if a, err := db.SigninApprovalByBrowser(ctx, "b"); err != nil || a.Status != store.SigninEnded {
		t.Fatalf("request: %+v %v", a, err)
	}
	db.Close() // release the SQLite lock for the next CLI runs

	if code, _, errs := runWithStdin(t, "", lookup, "user", "remove-pin", "--email", "ghost@example.com"); code != 1 || !strings.Contains(errs, "no user with email") {
		t.Fatalf("unknown user: code=%d %q", code, errs)
	}
	if code, _, _ := runWithStdin(t, "", lookup, "user", "remove-pin"); code == 0 {
		t.Fatal("missing --email accepted")
	}
}

func TestUserUsageNamesRemovePIN(t *testing.T) {
	if _, _, errs := runWithStdin(t, "", migrateEnv(t.TempDir()), "user"); !strings.Contains(errs, "paymux user remove-pin --email <email>") {
		t.Fatalf("usage: %q", errs)
	}
}
