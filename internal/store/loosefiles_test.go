package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Audit R-A17: a database file that others can read is reported, so the
// operator can chmod 600 it. Files the hub creates itself are 0600.
func TestLooseFilesReportsReadableDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paymux.db")
	db, err := openSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.writer.Exec(`CREATE TABLE t (x INT); INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	if got := db.LooseFiles(); len(got) != 0 {
		t.Fatalf("a database the hub created is loose: %v", got)
	}
	var want []string
	for _, side := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(path + side); err == nil {
			if err := os.Chmod(path+side, 0o644); err != nil {
				t.Fatal(err)
			}
			want = append(want, path+side)
		}
	}
	got := db.LooseFiles()
	if !slices.Contains(got, path) || len(got) != len(want) {
		t.Fatalf("LooseFiles = %v, want %v", got, want)
	}
	// Group-readable counts as well; owner-only does not.
	for _, f := range want {
		_ = os.Chmod(f, 0o640)
	}
	if got := db.LooseFiles(); len(got) != len(want) {
		t.Errorf("0640: %v", got)
	}
	for _, f := range want {
		_ = os.Chmod(f, 0o600)
	}
	if got := db.LooseFiles(); len(got) != 0 {
		t.Errorf("0600: %v", got)
	}
}
