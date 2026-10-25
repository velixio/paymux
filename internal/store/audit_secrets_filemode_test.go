//go:build unix

package store_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/paymux/paymux/internal/store"
)

// Audit 2026-10: the SQLite database (sealed secrets, API key hashes, session
// hashes, customer data) is created by the driver with the process umask. The
// data directory is created 0700 only when it does not exist yet; a directory
// that already exists (./data from .env.example, a pre-made volume) is not
// tightened, so with the common 022 umask the database is world-readable.
func TestAuditSQLiteFileMode(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), "", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, name := range []string{"paymux.db", "paymux.db-wal", "paymux.db-shm"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) && name != "paymux.db" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v: readable by group/others", name, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm()&0o077 != 0 {
		t.Logf("data dir stays %v", st.Mode().Perm())
	}
}
