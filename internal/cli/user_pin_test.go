package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
)

// approvalUser creates a@example.com with an authenticator, a linked Telegram
// account, PIN approval, a session and a pending request.
func approvalUser(t *testing.T, lookup func(string) (string, bool)) string {
	t.Helper()
	if code, _, errs := runWithStdin(t, cliPW+"\n", lookup, "user", "create", "--email", "a@example.com", "--role", "viewer", "--password-stdin"); code != 0 {
		t.Fatal(errs)
	}
	ctx := context.Background()
	now := time.Now()
	db := openDB(t, lookup)
	defer db.Close() // release the SQLite lock for the CLI runs
	u, _ := db.UserByEmail(ctx, "a@example.com")
	if err := db.SetUserTOTP(ctx, u.ID, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddTelegramLink(ctx, "org", u.ID, "code", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UseTelegramLink(ctx, "org", "code", 42, now); err != nil {
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
			Status: store.SigninPending, Method: store.SigninMethodPIN, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestUserRemoveSigninApprovalAndUnlinkTelegram(t *testing.T) {
	for _, tc := range []struct {
		cmd, out string
		linked   bool
	}{
		{"remove-signin-approval", "sign-in approval turned off for a@example.com; their pending approval requests were cancelled.\n", true},
		{"unlink-telegram", "Telegram disconnected and sign-in approval turned off for a@example.com; their pending approval requests were cancelled.\n", false},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			lookup := migrateEnv(t.TempDir())
			id := approvalUser(t, lookup)
			code, out, errs := runWithStdin(t, "", lookup, "user", tc.cmd, "--email", "A@example.com")
			if code != 0 {
				t.Fatalf("code=%d stderr=%q", code, errs)
			}
			if out != tc.out {
				t.Fatalf("stdout %q, want %q", out, tc.out)
			}
			ctx := context.Background()
			db := openDB(t, lookup)
			got, _ := db.UserByID(ctx, id)
			if got.SigninPINEnc != nil || got.SigninMethod != "" || (got.TelegramUserID != 0) != tc.linked {
				t.Fatalf("user %+v", got)
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
			db.Close()

			if code, _, errs := runWithStdin(t, "", lookup, "user", tc.cmd, "--email", "ghost@example.com"); code != 1 || !strings.Contains(errs, "no user with email") {
				t.Fatalf("unknown user: code=%d %q", code, errs)
			}
			if code, _, _ := runWithStdin(t, "", lookup, "user", tc.cmd); code == 0 {
				t.Fatal("missing --email accepted")
			}
		})
	}
}

func TestUserUsageNamesTheApprovalCommands(t *testing.T) {
	_, _, errs := runWithStdin(t, "", migrateEnv(t.TempDir()), "user")
	for _, want := range []string{"paymux user remove-signin-approval --email <email>", "paymux user unlink-telegram --email <email>"} {
		if !strings.Contains(errs, want) {
			t.Fatalf("usage lacks %q: %q", want, errs)
		}
	}
}
