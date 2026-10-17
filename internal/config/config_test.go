package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var goodKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
var goodHex = strings.Repeat("ab", 32) // 64 hex chars: valid base64 of 48 bytes

func env(kv ...string) func(string) (string, bool) {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// base returns a lookup with a valid base URL and master key, plus overrides.
func base(kv ...string) func(string) (string, bool) {
	all := []string{"PAYMUX_BASE_URL", "https://pay.example.com", "PAYMUX_MASTER_KEY", goodKey}
	return env(append(all, kv...)...) // later pairs override earlier ones
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("err = %v, want containing %q", err, sub)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != "production" || c.Listen != ":8080" || c.InternalListen != "127.0.0.1:9100" ||
		c.DataDir != "./data" || c.DatabaseURL != "sqlite://data/paymux.db" ||
		c.Location.String() != "Asia/Kolkata" || len(c.MasterKey) != 32 || c.DevSkip2FA {
		t.Fatalf("%+v", c)
	}
}

func TestMissingBaseURL(t *testing.T) {
	_, err := Load(env("PAYMUX_MASTER_KEY", goodKey))
	wantErr(t, err, "PAYMUX_BASE_URL is required")
}

func TestBaseURLRules(t *testing.T) {
	ok := []string{"http://localhost:8080", "http://127.0.0.1", "http://a.localhost", "http://shop.test", "https://pay.example.com"}
	for _, u := range ok {
		if _, err := Load(base("PAYMUX_BASE_URL", u)); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://pay.example.com", "ftp://x", "pay.example.com"} {
		_, err := Load(base("PAYMUX_BASE_URL", u))
		wantErr(t, err, "PAYMUX_BASE_URL")
	}
}

func TestMasterKeyBothSet(t *testing.T) {
	_, err := Load(base("PAYMUX_MASTER_KEY_FILE", "/x"))
	wantErr(t, err, "exactly one of PAYMUX_MASTER_KEY or PAYMUX_MASTER_KEY_FILE")
}

func TestMasterKeyNoneSet(t *testing.T) {
	_, err := Load(env("PAYMUX_BASE_URL", "https://pay.example.com"))
	wantErr(t, err, "exactly one of PAYMUX_MASTER_KEY or PAYMUX_MASTER_KEY_FILE")
}

func TestMasterKeyWrongLength(t *testing.T) {
	_, err := Load(base("PAYMUX_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16))))
	wantErr(t, err, "PAYMUX_MASTER_KEY must decode to 32 bytes, got 16")
}

func TestMasterKeyHexRejected(t *testing.T) {
	_, err := Load(base("PAYMUX_MASTER_KEY", goodHex))
	wantErr(t, err, "must decode to 32 bytes, got 48")
}

func TestMasterKeyBase64AndInvalid(t *testing.T) {
	b64 := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	c, err := Load(base("PAYMUX_MASTER_KEY", b64))
	if err != nil || len(c.MasterKey) != 32 {
		t.Fatalf("%v %v", c.MasterKey, err)
	}
	_, err = Load(base("PAYMUX_MASTER_KEY", "not a key!"))
	wantErr(t, err, "PAYMUX_MASTER_KEY")
	if strings.Contains(err.Error(), "not a key") {
		t.Error("error leaks key material")
	}
}

func TestMasterKeyFileTrimmed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(goodKey+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env("PAYMUX_BASE_URL", "https://pay.example.com", "PAYMUX_MASTER_KEY_FILE", p))
	if err != nil || len(c.MasterKey) != 32 {
		t.Fatalf("%v %v", c.MasterKey, err)
	}
	_, err = Load(env("PAYMUX_BASE_URL", "https://pay.example.com", "PAYMUX_MASTER_KEY_FILE", p+".missing"))
	wantErr(t, err, "PAYMUX_MASTER_KEY_FILE")
}

func TestBadTZ(t *testing.T) {
	_, err := Load(base("PAYMUX_TZ", "Mars/Base"))
	wantErr(t, err, "PAYMUX_TZ")
}

func TestBadEnv(t *testing.T) {
	_, err := Load(base("PAYMUX_ENV", "staging"))
	wantErr(t, err, "PAYMUX_ENV must be production or development")
}

func TestDatabaseURLForms(t *testing.T) {
	for _, u := range []string{"sqlite:///var/lib/paymux/p.db", "postgres://u:p@h:5432/db?sslmode=disable"} {
		c, err := Load(base("PAYMUX_DATABASE_URL", u))
		if err != nil || c.DatabaseURL != u {
			t.Errorf("%s: %q %v", u, c.DatabaseURL, err)
		}
	}
	_, err := Load(base("PAYMUX_DATABASE_URL", "mysql://x"))
	wantErr(t, err, "PAYMUX_DATABASE_URL must start with sqlite:// or postgres://")
}

func TestRedactedDatabaseURL(t *testing.T) {
	c := Config{DatabaseURL: "postgres://u:s3cret@h/db"}
	if got := c.RedactedDatabaseURL(); got != "postgres://u:xxxxx@h/db" {
		t.Fatal(got)
	}
	c = Config{DatabaseURL: "sqlite://data/paymux.db"}
	if got := c.RedactedDatabaseURL(); got != "sqlite://data/paymux.db" {
		t.Fatal(got)
	}
}

func TestDevSkip2FAGuard(t *testing.T) {
	const msg = "PAYMUX_DEV_SKIP_2FA is only allowed in development on a local host"
	const bad = "PAYMUX_DEV_SKIP_2FA must be true or false"
	cases := []struct {
		env, url, skip, err string
		want                bool
	}{
		{"development", "http://localhost:8080", "true", "", true},
		{"development", "https://pay.example.com", "true", msg, false},
		{"production", "http://localhost:8080", "true", msg, false},
		{"production", "https://pay.example.com", "", "", false},
		{"production", "https://pay.example.com", "false", "", false},
		{"development", "http://localhost:8080", "maybe", bad, false},
		{"production", "https://pay.example.com", "maybe", bad, false},
	}
	for _, tc := range cases {
		kv := []string{"PAYMUX_ENV", tc.env, "PAYMUX_BASE_URL", tc.url}
		if tc.skip != "" {
			kv = append(kv, "PAYMUX_DEV_SKIP_2FA", tc.skip)
		}
		c, err := Load(base(kv...))
		if tc.err != "" {
			wantErr(t, err, tc.err)
			continue
		}
		if err != nil || c.DevSkip2FA != tc.want {
			t.Errorf("%+v: %v skip=%v", tc, err, c.DevSkip2FA)
		}
	}
}

func TestAllowPrivateWebhooks(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"", false}, {"true", true}, {"false", false}} {
		kv := []string{}
		if tc.val != "" {
			kv = append(kv, "PAYMUX_ALLOW_PRIVATE_WEBHOOKS", tc.val)
		}
		c, err := Load(base(kv...))
		if err != nil || c.AllowPrivateWebhooks != tc.want {
			t.Errorf("%q: err=%v got=%v", tc.val, err, c.AllowPrivateWebhooks)
		}
	}
	_, err := Load(base("PAYMUX_ALLOW_PRIVATE_WEBHOOKS", "yes"))
	wantErr(t, err, "PAYMUX_ALLOW_PRIVATE_WEBHOOKS must be true or false")
}

func TestSMTPAndOpsDefaults(t *testing.T) {
	c, err := Load(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPHost != "" || c.SMTPPort != 587 || len(c.OpsEmails) != 0 || c.OpsWebhookURL != "" {
		t.Fatalf("%+v", c)
	}
}

func TestSMTPAndOpsParsed(t *testing.T) {
	c, err := Load(base("PAYMUX_SMTP_HOST", "smtp.example.com", "PAYMUX_SMTP_PORT", "2525",
		"PAYMUX_SMTP_USERNAME", "mailer", "PAYMUX_SMTP_PASSWORD", "hunter2-secret",
		"PAYMUX_SMTP_FROM", "Paymux <billing@example.com>",
		"PAYMUX_OPS_EMAILS", "ops@example.com, Oncall <oncall@example.com>",
		"PAYMUX_OPS_WEBHOOK_URL", "https://hooks.example.com/T/B/x"))
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPHost != "smtp.example.com" || c.SMTPPort != 2525 || c.SMTPUsername != "mailer" ||
		c.SMTPPassword.Reveal() != "hunter2-secret" || c.SMTPFrom != "Paymux <billing@example.com>" ||
		len(c.OpsEmails) != 2 || c.OpsEmails[0] != "ops@example.com" || c.OpsEmails[1] != "oncall@example.com" ||
		c.OpsWebhookURL != "https://hooks.example.com/T/B/x" {
		t.Fatalf("%+v", c)
	}
}

func TestSMTPPasswordIsRedacted(t *testing.T) {
	c, err := Load(base("PAYMUX_SMTP_HOST", "smtp.example.com", "PAYMUX_SMTP_FROM", "b@example.com", "PAYMUX_SMTP_PASSWORD", "hunter2-secret"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	l.Info("cfg", "cfg", c, "pw", c.SMTPPassword)
	j, _ := json.Marshal(c.SMTPPassword)
	for name, s := range map[string]string{"%v": fmt.Sprintf("%v", c), "%+v": fmt.Sprintf("%+v", c),
		"%#v": fmt.Sprintf("%#v", c), "%s": fmt.Sprint(c.SMTPPassword), "slog": buf.String(), "json": string(j)} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("%s leaks the password: %s", name, s)
		}
	}
}

func TestSMTPAndOpsValidation(t *testing.T) {
	cases := []struct {
		kv  []string
		msg string
	}{
		{[]string{"PAYMUX_SMTP_PORT", "abc"}, "PAYMUX_SMTP_PORT must be a port number"},
		{[]string{"PAYMUX_SMTP_PORT", "70000"}, "PAYMUX_SMTP_PORT must be a port number"},
		{[]string{"PAYMUX_SMTP_HOST", "smtp.example.com"}, "PAYMUX_SMTP_FROM is required when PAYMUX_SMTP_HOST is set"},
		{[]string{"PAYMUX_SMTP_HOST", "smtp.example.com", "PAYMUX_SMTP_FROM", "not an address"}, "PAYMUX_SMTP_FROM"},
		{[]string{"PAYMUX_OPS_EMAILS", "ok@example.com,nope"}, "PAYMUX_OPS_EMAILS"},
		{[]string{"PAYMUX_OPS_WEBHOOK_URL", "ftp://example.com"}, "PAYMUX_OPS_WEBHOOK_URL must be an absolute http(s) URL"},
	}
	for _, tc := range cases {
		_, err := Load(base(tc.kv...))
		wantErr(t, err, tc.msg)
	}
}

func TestAllProblemsReported(t *testing.T) {
	_, err := Load(env("PAYMUX_TZ", "Mars/Base", "PAYMUX_ENV", "staging"))
	for _, s := range []string{"PAYMUX_TZ", "PAYMUX_ENV", "PAYMUX_BASE_URL is required", "PAYMUX_MASTER_KEY"} {
		wantErr(t, err, s)
	}
}

func TestJSHooksConfig(t *testing.T) {
	c, err := Load(base("PAYMUX_DATA_DIR", "/var/lib/paymux"))
	if err != nil || c.JSHooksDir != "/var/lib/paymux/hooks" || len(c.JSHooksAllowedHosts) != 0 {
		t.Fatalf("defaults: %v %q %v", err, c.JSHooksDir, c.JSHooksAllowedHosts)
	}
	c, err = Load(base("PAYMUX_JS_HOOKS_DIR", "/etc/paymux/hooks", "PAYMUX_JS_HOOKS_ALLOWED_HOSTS", " Hooks.Slack.com, *.example.com ,api.example.org "))
	if err != nil || c.JSHooksDir != "/etc/paymux/hooks" {
		t.Fatalf("explicit: %v %q", err, c.JSHooksDir)
	}
	want := []string{"hooks.slack.com", "*.example.com", "api.example.org"}
	if len(c.JSHooksAllowedHosts) != 3 || c.JSHooksAllowedHosts[0] != want[0] || c.JSHooksAllowedHosts[1] != want[1] || c.JSHooksAllowedHosts[2] != want[2] {
		t.Errorf("allowed hosts %v", c.JSHooksAllowedHosts)
	}
	for _, bad := range []string{"https://hooks.slack.com", "hooks.slack.com/path", "a b", "*", "host:8443"} {
		_, err := Load(base("PAYMUX_JS_HOOKS_ALLOWED_HOSTS", bad))
		wantErr(t, err, "PAYMUX_JS_HOOKS_ALLOWED_HOSTS")
	}
}
