package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/auth"
	"github.com/velixio/paymux/internal/config"
	"github.com/velixio/paymux/internal/store"
)

const cliPW = "a long enough password"

func runWithStdin(t *testing.T, in string, lookup func(string) (string, bool), args ...string) (int, string, string) {
	t.Helper()
	old := Stdin
	Stdin = io.Reader(strings.NewReader(in))
	defer func() { Stdin = old }()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, lookup, &out, &errb)
	return code, out.String(), errb.String()
}

func openDB(t *testing.T, lookup func(string) (string, bool)) *store.DB {
	t.Helper()
	cfg, err := config.Load(lookup)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), cfg.DatabaseURL, cfg.DataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestUserCreateFromStdin(t *testing.T) {
	lookup := migrateEnv(t.TempDir())
	code, out, errs := runWithStdin(t, cliPW+"\n", lookup, "user", "create", "--email", "Admin@Example.com", "--role", "admin", "--password-stdin")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	if strings.Contains(out+errs, cliPW) {
		t.Fatal("password echoed")
	}
	if !strings.Contains(out, "admin@example.com") || !strings.Contains(out, "two-factor") {
		t.Fatalf("stdout=%q", out)
	}
	db := openDB(t, lookup)
	defer db.Close()
	u, err := db.UserByEmail(context.Background(), "admin@example.com")
	if err != nil || u.Role != "admin" || u.Disabled || u.TOTPSecretEnc != nil {
		t.Fatalf("%+v %v", u, err)
	}
	if ok, _ := auth.VerifyPassword(u.PasswordHash, cliPW); !ok {
		t.Fatal("stored hash does not match the password")
	}
}

func TestUserCreateRejections(t *testing.T) {
	lookup := migrateEnv(t.TempDir())
	create := func(in string, args ...string) (int, string) {
		code, _, errs := runWithStdin(t, in, lookup, append([]string{"user", "create"}, args...)...)
		return code, errs
	}
	if code, _ := create(cliPW+"\n", "--email", "a@example.com", "--role", "admin", "--password-stdin"); code != 0 {
		t.Fatal("setup failed")
	}
	cases := []struct {
		name string
		in   string
		args []string
		want string
	}{
		{"duplicate", cliPW + "\n", []string{"--email", "a@example.com", "--role", "viewer", "--password-stdin"}, "already exists"},
		{"weak password", "short\n", []string{"--email", "b@example.com", "--role", "viewer", "--password-stdin"}, "at least 12"},
		{"bad role", cliPW + "\n", []string{"--email", "c@example.com", "--role", "root", "--password-stdin"}, "unknown role"},
		{"bad email", cliPW + "\n", []string{"--email", "nope", "--role", "viewer", "--password-stdin"}, "valid email"},
		{"missing email", cliPW + "\n", []string{"--role", "viewer", "--password-stdin"}, "--email"},
		{"empty stdin", "", []string{"--email", "d@example.com", "--role", "viewer", "--password-stdin"}, "password"},
		{"no tty and no flag", cliPW + "\n", []string{"--email", "e@example.com", "--role", "viewer"}, "--password-stdin"},
	}
	for _, c := range cases {
		code, errs := create(c.in, c.args...)
		if code == 0 || !strings.Contains(errs, c.want) {
			t.Errorf("%s: code=%d stderr=%q (want %q)", c.name, code, errs, c.want)
		}
	}
	db := openDB(t, lookup)
	defer db.Close()
	if n, _ := db.CountUsers(context.Background()); n != 1 {
		t.Fatalf("users=%d", n)
	}
}

func TestUserResetTOTP(t *testing.T) {
	lookup := migrateEnv(t.TempDir())
	if code, _, errs := runWithStdin(t, cliPW+"\n", lookup, "user", "create", "--email", "a@example.com", "--role", "viewer", "--password-stdin"); code != 0 {
		t.Fatal(errs)
	}
	ctx := context.Background()
	db := openDB(t, lookup)
	u, _ := db.UserByEmail(ctx, "a@example.com")
	_ = db.SetUserTOTP(ctx, u.ID, []byte{1, 2, 3})
	_, _ = db.AdvanceTOTPStep(ctx, u.ID, 77)
	now := time.Now()
	_ = db.InsertSession(ctx, store.SessionRow{ID: "ses_1", UserID: u.ID, TokenHash: "h", CSRFKey: []byte{1}, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(time.Hour), IP: "x", UserAgent: "x"})
	db.Close()

	code, out, errs := runWithStdin(t, "", lookup, "user", "reset-2fa", "--email", "A@example.com")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	if !strings.Contains(out, "a@example.com") || !enrolCodeRE.MatchString(out) {
		t.Fatalf("stdout=%q: no enrolment code", out)
	}
	db = openDB(t, lookup)
	u, _ = db.UserByID(ctx, u.ID)
	if u.TOTPSecretEnc != nil || u.TOTPLastStep != -1 {
		t.Fatalf("not reset: %+v", u)
	}
	if _, _, err := db.SessionByTokenHash(ctx, "h"); err == nil {
		t.Fatal("sessions survive a 2FA reset")
	}
	db.Close() // release the SQLite lock for the next CLI runs
	if code, _, errs := runWithStdin(t, "", lookup, "user", "reset-2fa", "--email", "ghost@example.com"); code == 0 || !strings.Contains(errs, "no user") {
		t.Fatalf("unknown user: code=%d %q", code, errs)
	}
	if code, _, _ := runWithStdin(t, "", lookup, "user", "reset-2fa"); code == 0 {
		t.Fatal("missing --email accepted")
	}
}

func TestUserUsage(t *testing.T) {
	for _, args := range [][]string{{"user"}, {"user", "bogus"}} {
		code, _, errs := runWithStdin(t, "", migrateEnv(t.TempDir()), args...)
		if code != 2 || !strings.Contains(errs, "usage: paymux user") {
			t.Errorf("%v: code=%d %q", args, code, errs)
		}
	}
}

var enrolCodeRE = regexp.MustCompile(`[a-z]{6}(-[a-z]{6}){3}`)

// The person is told the one-time code that lets them set up two-factor sign-in.
func TestUserCreatePrintsTheEnrolmentCode(t *testing.T) {
	lookup := migrateEnv(t.TempDir())
	code, out, errs := runWithStdin(t, cliPW+"\n", lookup, "user", "create", "--email", "n@example.com", "--role", "viewer", "--password-stdin")
	if code != 0 || !enrolCodeRE.MatchString(out) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errs)
	}
	db := openDB(t, lookup)
	defer db.Close()
	u, _ := db.UserByEmail(context.Background(), "n@example.com")
	if ok, _ := db.HasEnrolmentToken(context.Background(), u.ID); !ok {
		t.Error("no code stored")
	}
}
