package config

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var goodKey = strings.Repeat("ab", 32) // 64 hex chars = 32 bytes

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
	_, err := Load(base("PAYMUX_MASTER_KEY", hex.EncodeToString(make([]byte, 16))))
	wantErr(t, err, "PAYMUX_MASTER_KEY must decode to 32 bytes, got 16")
}

func TestMasterKeyBase64AndInvalid(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
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

func TestAllProblemsReported(t *testing.T) {
	_, err := Load(env("PAYMUX_TZ", "Mars/Base", "PAYMUX_ENV", "staging"))
	for _, s := range []string{"PAYMUX_TZ", "PAYMUX_ENV", "PAYMUX_BASE_URL is required", "PAYMUX_MASTER_KEY"} {
		wantErr(t, err, s)
	}
}
