package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/migrations"
)

func migrateDir(t *testing.T, dir string) func(string) (string, bool) {
	t.Helper()
	lookup := migrateEnv(dir)
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"migrate"}, lookup, &out, &errb); code != 0 {
		t.Fatalf("migrate: %q", errb.String())
	}
	return lookup
}

func TestBackupCommandWritesAStandaloneCopy(t *testing.T) {
	dir := t.TempDir()
	lookup := migrateDir(t, dir)
	dest := filepath.Join(t.TempDir(), "paymux-backup.db")
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"backup", "--to", dest}, lookup, &out, &errb); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), dest) {
		t.Errorf("stdout=%q", out.String())
	}
	db, err := store.Open(context.Background(), "sqlite://"+dest, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, err := db.PendingMigrations(context.Background(), migrations.FS); err != nil || n != 0 {
		t.Errorf("backup is missing migrations: %d %v", n, err)
	}
	out.Reset()
	errb.Reset()
	if code := Run(context.Background(), []string{"backup", "--to", dest}, lookup, &out, &errb); code != 1 || !strings.Contains(errb.String(), "already exists") {
		t.Errorf("second backup: code=%d stderr=%q", code, errb.String())
	}
	if code := Run(context.Background(), []string{"backup"}, lookup, &out, &errb); code != 2 {
		t.Errorf("missing --to: code=%d", code)
	}
}

func TestDBCopyCommand(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	lookup := migrateDir(t, src)
	migrateDir(t, dst)
	from, to := "sqlite://"+filepath.Join(src, "paymux.db"), "sqlite://"+filepath.Join(dst, "paymux.db")
	seed, err := store.Open(context.Background(), from, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Write(context.Background(), func(tx store.Tx) error {
		return store.SeedOrg(context.Background(), tx, "Acme", time.UnixMilli(1_700_000_000_000))
	}); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"db", "copy", "--from", from, "--to", to}, lookup, &out, &errb); code != 0 {
		t.Fatalf("empty target: code=%d stderr=%q", code, errb.String())
	}
	for _, want := range []string{"orgs", "payments", "total", "ok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q: %q", want, out.String())
		}
	}
	out.Reset()
	errb.Reset()
	// The first copy put the org in the target, so a second is refused.
	code := Run(context.Background(), []string{"db", "copy", "--from", from, "--to", to}, lookup, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "--force") {
		t.Fatalf("non-empty target: code=%d stderr=%q", code, errb.String())
	}
	if code := Run(context.Background(), []string{"db", "copy", "--from", from, "--to", to, "--force"}, lookup, &out, &errb); code != 0 {
		t.Fatalf("forced: code=%d stderr=%q", code, errb.String())
	}
	if code := Run(context.Background(), []string{"db", "copy", "--from", from}, lookup, &out, &errb); code != 2 {
		t.Errorf("missing --to: code=%d", code)
	}
	if code := Run(context.Background(), []string{"db", "copy", "--from", from, "--to", from}, lookup, &out, &errb); code != 1 {
		t.Errorf("same db: code=%d", code)
	}
}
