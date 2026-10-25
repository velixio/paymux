package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Audit R-A17: starting on database files that others can read logs a warning
// that names chmod 600, for the main database and for logs.db. The files keep
// their mode: a backup agent may rely on it.
func TestStartWarnsAboutReadableDatabaseFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"paymux.db", "logs.db"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	r := startApp(t, cfg)
	waitReady(t, r)
	out := r.logs.String()
	for _, f := range []string{"paymux.db", "logs.db"} {
		if !strings.Contains(out, "readable by other users") || !strings.Contains(out, filepath.Join(dir, f)) || !strings.Contains(out, "chmod 600") {
			t.Errorf("no warning naming %s and chmod 600:\n%s", f, out)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "paymux.db")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the hub changed the mode of the database: %v %v", fi.Mode(), err)
	}
	shutdown(t, r)
}

func TestStartIsQuietAboutDatabaseFilesItCreated(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	if out := r.logs.String(); strings.Contains(out, "readable by other users") {
		t.Errorf("warned about files the hub created:\n%s", out)
	}
	shutdown(t, r)
}
