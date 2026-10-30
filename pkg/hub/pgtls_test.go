package hub

import (
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/logging"
)

// The startup warning is logged for a remote PostgreSQL host without
// verify-full, and never carries the password.
func TestStartupWarnsAboutUnverifiedDatabaseTLS(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	var out syncBuf
	app, err := New(cfg, WithLogger(logging.New(&out, -4)))
	if err != nil {
		t.Fatal(err)
	}
	app.warnDatabaseTLS() // SQLite: nothing to say
	if out.String() != "" {
		t.Errorf("warned on SQLite: %q", out.String())
	}
	app.cfg.DatabaseURL = "postgres://u:secretpw@db.internal:5432/paymux?sslmode=require"
	app.warnDatabaseTLS()
	got := out.String()
	if !strings.Contains(got, "verify-full") || !strings.Contains(got, "WARN") || strings.Contains(got, "secretpw") {
		t.Errorf("warning = %q", got)
	}
	out2 := syncBuf{}
	app.log = logging.New(&out2, -4)
	app.cfg.DatabaseURL = "postgres://u:secretpw@db.internal:5432/paymux?sslmode=verify-full"
	app.warnDatabaseTLS()
	if out2.String() != "" {
		t.Errorf("warned on verify-full: %q", out2.String())
	}
}
