package cli

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/store"
)

func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func queryOne(t *testing.T, dir, query string, dst ...any) {
	t.Helper()
	db, err := store.Open(context.Background(), "", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Read(context.Background(), func(q store.Querier) error {
		return q.QueryRowContext(context.Background(), query).Scan(dst...)
	})
	if err != nil {
		t.Fatal(err)
	}
}

var webhookRE = regexp.MustCompile(`http://localhost:8080/hooks/(pa_[0-9a-z]{26})`)

func TestProviderAddPrintsWebhookURLAndLinksApp(t *testing.T) {
	dir := t.TempDir()
	fake := fakeconn.Register("fake")
	if code, _, errs := runIn(dir, "app", "create", "--name", "Shop", "--slug", "shop"); code != 0 {
		t.Fatal(errs)
	}
	cfg := writeCfg(t, dir, fakeconn.ValidConfig)
	code, out, errs := runIn(dir, "provider", "add", "--connector", fake.Name, "--name", "main", "--mode", "test",
		"--config-file", cfg, "--currencies", "INR,USD", "--app", "shop")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	m := webhookRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no webhook URL in %q", out)
	}
	var currencies string
	var linked int
	queryOne(t, dir, "SELECT accepted_currencies FROM provider_accounts WHERE id = '"+m[1]+"'", &currencies)
	queryOne(t, dir, "SELECT COUNT(*) FROM app_provider_accounts WHERE provider_account_id = '"+m[1]+"'", &linked)
	if currencies != `["INR","USD"]` || linked != 1 {
		t.Errorf("currencies=%s linked=%d", currencies, linked)
	}
	var enc []byte
	queryOne(t, dir, "SELECT config_enc FROM provider_accounts WHERE id = '"+m[1]+"'", &enc)
	if len(enc) == 0 || strings.Contains(string(enc), "fake_key_123") || strings.Contains(out+errs, "fake_key_123") {
		t.Error("config must be stored encrypted and never printed")
	}
	if !strings.Contains(out, "Register this URL") {
		t.Errorf("output should tell the operator to register the webhook URL: %q", out)
	}
}

func TestProviderAddWithoutAppWarns(t *testing.T) {
	dir := t.TempDir()
	fake := fakeconn.Register("fake")
	cfg := writeCfg(t, dir, fakeconn.ValidConfig)
	code, out, errs := runIn(dir, "provider", "add", "--connector", fake.Name, "--name", "main", "--mode", "live", "--config-file", cfg)
	if code != 0 || webhookRE.FindStringSubmatch(out) == nil {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(out, "not linked to any app") {
		t.Errorf("expected a hint that no app can use the account yet: %q", out)
	}
}

func TestProviderAddErrors(t *testing.T) {
	dir := t.TempDir()
	fake := fakeconn.Register("fake")
	if code, _, errs := runIn(dir, "app", "create", "--name", "Shop", "--slug", "shop"); code != 0 {
		t.Fatal(errs)
	}
	good := writeCfg(t, dir, fakeconn.ValidConfig)
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"api_key":""}`), 0o600)
	base := []string{"provider", "add", "--connector", fake.Name, "--name", "main", "--mode", "test", "--config-file", good}
	with := func(over map[string]string) []string {
		args := append([]string{}, base...)
		for i := 2; i < len(args); i += 2 {
			if v, ok := over[strings.TrimPrefix(args[i], "--")]; ok {
				args[i+1] = v
			}
		}
		return args
	}
	cases := map[string]struct {
		args []string
		code int
		msg  string
	}{
		"missing config file": {with(map[string]string{"config-file": filepath.Join(dir, "none.json")}), 1, "config"},
		"invalid config":      {with(map[string]string{"config-file": bad}), 1, "api_key"},
		"unknown connector":   {with(map[string]string{"connector": "nope"}), 1, "unknown connector"},
		"bad mode":            {with(map[string]string{"mode": "prod"}), 1, "mode"},
		"unknown app":         {append(with(nil), "--app", "ghost"), 1, "not found"},
		"unsupported cur":     {append(with(nil), "--currencies", "JPY"), 1, "JPY"},
		"no connector":        {[]string{"provider", "add", "--name", "x", "--mode", "test", "--config-file", good}, 2, "usage"},
		"no subcommand":       {[]string{"provider"}, 2, "usage"},
		"stray argument":      {append(with(nil), "stray"), 2, "usage"},
	}
	for name, c := range cases {
		code, out, errs := runIn(dir, c.args...)
		if code != c.code || !strings.Contains(errs, c.msg) || webhookRE.MatchString(out) {
			t.Errorf("%s: code=%d out=%q err=%q", name, code, out, errs)
		}
	}
	var n int
	queryOne(t, dir, "SELECT COUNT(*) FROM provider_accounts", &n)
	if n != 0 {
		t.Errorf("%d accounts created by failing commands", n)
	}
}

func TestDomainAdd(t *testing.T) {
	dir := t.TempDir()
	if code, _, errs := runIn(dir, "app", "create", "--name", "Shop", "--slug", "shop"); code != 0 {
		t.Fatal(errs)
	}
	code, out, errs := runIn(dir, "domain", "add", "--app", "shop", "--host", "Pay.Shop.Example.org")
	if code != 0 || !strings.Contains(out, "pay.shop.example.org") || !strings.Contains(out, "/.well-known/paymux/") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	var token string
	queryOne(t, dir, "SELECT token FROM app_domains WHERE host = 'pay.shop.example.org'", &token)
	if len(token) < 32 || !strings.Contains(out, token) {
		t.Errorf("token %q not shown in %q", token, out)
	}
	if code, _, errs := runIn(dir, "domain", "add", "--app", "shop", "--host", "pay.shop.example.org"); code != 1 || !strings.Contains(errs, "already registered") {
		t.Errorf("duplicate: %d %q", code, errs)
	}
	if code, _, errs := runIn(dir, "domain", "add", "--app", "shop", "--host", "not a host"); code != 1 || !strings.Contains(errs, "valid host") {
		t.Errorf("invalid host: %d %q", code, errs)
	}
	if code, _, errs := runIn(dir, "domain", "add", "--app", "ghost", "--host", "x.example.org"); code != 1 || !strings.Contains(errs, "not found") {
		t.Errorf("unknown app: %d %q", code, errs)
	}
	if code, _, errs := runIn(dir, "domain", "add", "--app", "shop", "--host", "localhost"); code != 1 {
		t.Errorf("single-label host: %d %q", code, errs)
	}
	if code, _, _ := runIn(dir, "domain", "add", "--app", "shop"); code != 2 {
		t.Errorf("missing host: %d", code)
	}
}
