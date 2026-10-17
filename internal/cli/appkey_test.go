package cli

import (
	"reflect"
	"strings"
	"testing"
)

func keyOf(t *testing.T, out string) string {
	t.Helper()
	m := keyLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no key in %q", out)
	}
	return m[1]
}

func TestAppKeyWithScopes(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runIn(dir, "app", "create", "--name", "A", "--slug", "a"); code != 0 {
		t.Fatal(errs)
	}
	code, out, errs := runIn(dir, "app", "key", "--app", "a", "--mode", "live", "--scopes", "refunds:write,payments:read")
	if code != 0 {
		t.Fatalf("code=%d %s", code, errs)
	}
	if !strings.Contains(out, "scopes: payments:read,refunds:write") {
		t.Errorf("scopes not reported: %q", out)
	}
	if pr := authenticate(t, dir, keyOf(t, out)); !reflect.DeepEqual(pr.Scopes, []string{"payments:read", "refunds:write"}) {
		t.Errorf("scopes %v", pr.Scopes)
	}
	// Without --scopes the key has them all (and nothing is printed about scopes).
	_, out, _ = runIn(dir, "app", "key", "--app", "a")
	if pr := authenticate(t, dir, keyOf(t, out)); pr.Scopes != nil || strings.Contains(out, "scopes:") {
		t.Errorf("default key: scopes %v out %q", pr.Scopes, out)
	}
	if code, _, errs := runIn(dir, "app", "key", "--app", "a", "--scopes", "payments:delete"); code != 1 || !strings.Contains(errs, "unknown scope") {
		t.Errorf("bad scope: code=%d %q", code, errs)
	}
}

func TestAppKeyRotate(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runIn(dir, "app", "create", "--name", "A", "--slug", "a", "--mode", "live"); code != 0 {
		t.Fatal(errs)
	}
	_, out, _ := runIn(dir, "app", "key", "--app", "a", "--mode", "live", "--scopes", "invoices:read")
	old := keyOf(t, out)

	code, out, errs := runIn(dir, "app", "key", "--app", "a", "--mode", "live", "--rotate", "--grace-hours", "6")
	if code != 0 {
		t.Fatalf("rotate: code=%d %s", code, errs)
	}
	fresh := keyOf(t, out)
	if fresh == old || !strings.Contains(out, "stops working at") || !strings.Contains(out, "scopes: invoices:read") ||
		strings.Count(out, "previous key") != 2 { // the key made by "app create" and the scoped one
		t.Errorf("output %q", out)
	}
	// Both keys work during the grace period.
	if pr := authenticate(t, dir, fresh); pr.Mode != "live" {
		t.Errorf("new key %+v", pr)
	}
	authenticate(t, dir, old)

	// Rotation without a previous key of that mode, bad input.
	if code, out, _ := runIn(dir, "app", "key", "--app", "a", "--mode", "test", "--rotate"); code != 0 || !strings.Contains(out, "no previous key") {
		t.Errorf("first test key by rotation: %d %q", code, out)
	}
	if code, _, errs := runIn(dir, "app", "key", "--app", "a", "--rotate", "--grace-hours", "0"); code != 1 || !strings.Contains(errs, "grace-hours") {
		t.Errorf("zero grace: %d %q", code, errs)
	}
	if code, _, _ := runIn(dir, "app", "create", "--name", "B", "--slug", "b", "--rotate"); code != 2 {
		t.Errorf("--rotate on create: %d", code)
	}
}
