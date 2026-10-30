package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/domains"
)

func TestThemesAreServedAndNewOnesApplyWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)
	addr := r.app.Addr()

	if code, body := fetch(t, addr, "/theme/default/theme.css"); code != 200 || !strings.Contains(body, "--color-accent") {
		t.Fatalf("default theme: %d %s", code, body)
	}
	if code, body := fetch(t, addr, "/theme/minimal-dark/theme.css"); code != 200 || !strings.Contains(body, "--color-bg") {
		t.Fatalf("minimal-dark theme: %d %s", code, body)
	}
	if code, _ := fetch(t, addr, "/theme/nope/theme.css"); code != 404 {
		t.Fatalf("unknown theme: %d", code)
	}
	if code, body := fetch(t, addr, "/dashboard/login"); code != 200 || !strings.Contains(body, `/theme/default/theme.css`) {
		t.Fatalf("the login page should link the default theme (%d)", code)
	}

	// A theme dropped into <data>/themes is served while the hub keeps running.
	themeDir := filepath.Join(dir, "themes", "mine")
	if err := os.MkdirAll(themeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"mine","version":"1.0.0","tokens":{"--color-accent":"#123456"}}`
	if err := os.WriteFile(filepath.Join(themeDir, "theme.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body := fetch(t, addr, "/theme/mine/theme.css"); code != 200 || !strings.Contains(body, "#123456") {
		t.Fatalf("new theme: %d %s", code, body)
	}
	shutdown(t, r)
}

func TestThemeRoutesAreCustomerRoutes(t *testing.T) {
	// Custom domains serve customer pages, which link their theme.
	for _, p := range []string{"/theme/default/theme.css", "/theme/mine/assets/logo.svg"} {
		if !domains.IsCustomerRoute(p) {
			t.Errorf("%s must be served on custom domains", p)
		}
	}
}
