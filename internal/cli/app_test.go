package cli

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
)

func runIn(dir string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, migrateEnv(dir), &out, &errb)
	return code, out.String(), errb.String()
}

var keyLine = regexp.MustCompile(`(?m)^(pmx_(?:test|live)_sk_[0-9A-Za-z]{43})$`)

func authenticate(t *testing.T, dir, key string) core.Principal {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, "", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.NewService(core.Options{DB: db, Settings: settings.New(db)})
	pr, err := svc.Authenticate(ctx, key)
	if err != nil {
		t.Fatalf("printed key does not authenticate: %v", err)
	}
	return pr
}

func TestAppCreatePrintsKeyOnce(t *testing.T) {
	dir := t.TempDir()
	code, out, errs := runIn(dir, "app", "create", "--name", "Voice Studio", "--slug", "voicestudio")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	m := keyLine.FindStringSubmatch(out)
	if m == nil || !strings.HasPrefix(m[1], "pmx_test_sk_") {
		t.Fatalf("no test key on its own line in %q (default mode is test)", out)
	}
	if !strings.Contains(out, "voicestudio") || !strings.Contains(out, "only once") {
		t.Errorf("output should name the app and warn the key is shown once: %q", out)
	}
	if strings.Count(out, "pmx_") != 1 {
		t.Errorf("exactly one key expected: %q", out)
	}
	if pr := authenticate(t, dir, m[1]); pr.Mode != "test" || pr.AppID == "" {
		t.Errorf("principal %+v", pr)
	}
	if strings.Contains(errs, "pmx_") {
		t.Errorf("key leaked to stderr: %q", errs)
	}
}

func TestAppCreateLiveModeAndDuplicateSlug(t *testing.T) {
	dir := t.TempDir()
	code, out, errs := runIn(dir, "app", "create", "--name", "A", "--slug", "a", "--mode", "live")
	if code != 0 || !strings.Contains(out, "pmx_live_sk_") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	code, _, errs = runIn(dir, "app", "create", "--name", "B", "--slug", "a")
	if code != 1 || !strings.Contains(errs, "already exists") {
		t.Fatalf("duplicate slug: code=%d err=%q", code, errs)
	}
}

func TestAppKeyForExistingApp(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runIn(dir, "app", "create", "--name", "A", "--slug", "a"); code != 0 {
		t.Fatal(errs)
	}
	code, out, errs := runIn(dir, "app", "key", "--app", "a", "--mode", "live")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	m := keyLine.FindStringSubmatch(out)
	if m == nil || !strings.HasPrefix(m[1], "pmx_live_sk_") {
		t.Fatalf("out=%q", out)
	}
	if pr := authenticate(t, dir, m[1]); pr.Mode != "live" {
		t.Errorf("principal %+v", pr)
	}
	if code, _, errs := runIn(dir, "app", "key", "--app", "nope", "--mode", "live"); code != 1 || !strings.Contains(errs, "not found") {
		t.Errorf("unknown app: code=%d err=%q", code, errs)
	}
}

func TestAppUsageErrors(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"no subcommand":  {"app"},
		"unknown sub":    {"app", "delete"},
		"missing name":   {"app", "create", "--slug", "x"},
		"missing slug":   {"app", "create", "--name", "X"},
		"key needs app":  {"app", "key", "--mode", "live"},
		"unknown flag":   {"app", "create", "--name", "X", "--slug", "x", "--bogus", "1"},
		"extra argument": {"app", "create", "--name", "X", "--slug", "x", "stray"},
	} {
		if code, out, _ := runIn(dir, args...); code != 2 || strings.Contains(out, "pmx_") {
			t.Errorf("%s: code=%d out=%q", name, code, out)
		}
	}
	if code, out, errs := runIn(dir, "app", "create", "--name", "X", "--slug", "x", "--mode", "prod"); code != 1 || strings.Contains(out, "pmx_") || !strings.Contains(errs, "mode") {
		t.Errorf("bad mode: code=%d out=%q err=%q", code, out, errs)
	}
	if code, _, errs := runIn(dir, "app", "create", "--name", "X", "--slug", "Bad Slug"); code != 1 || !strings.Contains(errs, "slug") {
		t.Errorf("bad slug: code=%d err=%q", code, errs)
	}
}
