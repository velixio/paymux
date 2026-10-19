//go:build unix

package store_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A database copy holds every customer and every sealed secret: it is created
// readable by its owner only, not created with the process umask and tightened
// afterwards, and the pre-migration snapshot is no different.
func TestBackupsAreOwnerOnlyEvenWithAPermissiveUmask(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := t.TempDir()
	db, err := openSQLite(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, both(map[string]string{"0001_a.sql": twoMigrations["0001_a.sql"]})); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, both(twoMigrations)); err != nil {
		t.Fatal(err)
	}
	snaps := backups(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("snapshots %v", snaps)
	}
	if st, _ := os.Stat(snaps[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("pre-migration snapshot mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(snaps[0])); st.Mode().Perm() != 0o700 {
		t.Errorf("backups directory mode %v", st.Mode().Perm())
	}
	dest := filepath.Join(dir, "out", "copy.db")
	if err := db.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dest); st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(dest)); st.Mode().Perm() != 0o700 {
		t.Errorf("backup directory mode %v", st.Mode().Perm())
	}
}
