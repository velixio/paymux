package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/paymux/paymux/internal/config"
	"github.com/paymux/paymux/internal/dashboard"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
)

// Stdin is where passwords are read from; tests replace it.
var Stdin io.Reader = os.Stdin

const userUsage = "usage: paymux user create --email <email> [--role <role>] [--password-stdin]\n" +
	"       paymux user reset-2fa --email <email>"

// runUser handles "paymux user ...". Like migrate, it opens the database
// directly, so with SQLite it cannot run while "paymux serve" holds the lock.
func runUser(ctx context.Context, args []string, env func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "create" && args[0] != "reset-2fa") {
		fmt.Fprintln(stderr, userUsage)
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("user "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "user email")
	role := fs.String("role", dashboard.RoleViewer, "admin, operator, viewer or a custom role name")
	pwStdin := fs.Bool("password-stdin", false, "read the password from standard input")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, userUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "paymux user %s: %v\n", cmd, err)
		return 1
	}
	if strings.TrimSpace(*email) == "" {
		return fail(errors.New("--email is required"))
	}

	password := ""
	if cmd == "create" {
		var err error
		if password, err = readPassword(*pwStdin, stderr); err != nil {
			return fail(err)
		}
	}

	cfg, err := config.Load(env)
	if err != nil {
		return fail(err)
	}
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DataDir, slog.New(slog.NewTextHandler(stderr, nil)))
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return fail(err)
	}
	users := dashboard.Users{DB: db}

	if cmd == "create" {
		u, err := users.Create(ctx, *email, *role, password)
		if errors.Is(err, dashboard.ErrInvalid) {
			return fail(errors.New(strings.TrimPrefix(err.Error(), dashboard.ErrInvalid.Error()+": ")))
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "created %s %s (%s)\nThey set up two-factor authentication at first login.\n", u.Role, u.Email, u.ID)
		return 0
	}
	if err := users.ResetTOTP(ctx, *email); errors.Is(err, store.ErrNotFound) {
		return fail(fmt.Errorf("no user with email %s", strings.ToLower(strings.TrimSpace(*email))))
	} else if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "two-factor authentication reset for %s; they were signed out and will enrol again at next login.\n",
		strings.ToLower(strings.TrimSpace(*email)))
	return 0
}

// readPassword reads the new password: one line from stdin with
// --password-stdin, otherwise an echo-free prompt (twice) on a terminal.
func readPassword(fromStdin bool, stderr io.Writer) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(Stdin).ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", errors.New("password is empty: pipe it on standard input")
		}
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			return "", errors.New("password is empty")
		}
		return pw, nil
	}
	f, ok := Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("no terminal to prompt on; use --password-stdin")
	}
	fmt.Fprint(stderr, "Password: ")
	a, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(stderr, "Repeat password: ")
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords do not match")
	}
	return string(a), nil
}
