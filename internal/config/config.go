// Package config loads Paymux bootstrap configuration from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Env is the deployment environment.
type Env string

const (
	EnvProduction  Env = "production"
	EnvDevelopment Env = "development"
)

// Config is the validated bootstrap configuration.
type Config struct {
	Env                             Env
	BaseURL                         *url.URL
	Listen, InternalListen, DataDir string
	DatabaseURL                     string // normalised; sqlite default resolved against DataDir
	MasterKey                       []byte // 32 bytes
	Location                        *time.Location
	DevSkip2FA                      bool
	AllowPrivateWebhooks            bool // PAYMUX_ALLOW_PRIVATE_WEBHOOKS: outbound webhooks may target private IPs

	// SMTP for customer and ops email. An empty SMTPHost disables email.
	SMTPHost, SMTPUsername, SMTPFrom string
	SMTPPort                         int // default 587 (STARTTLS)
	SMTPPassword                     Secret

	OpsEmails     []string // PAYMUX_OPS_EMAILS, bare addresses
	OpsWebhookURL string   // PAYMUX_OPS_WEBHOOK_URL, Slack-compatible

	// JavaScript hooks (plugins/jsvm). They run only when JSHooksDir exists.
	JSHooksDir          string   // PAYMUX_JS_HOOKS_DIR, default <data dir>/hooks
	JSHooksAllowedHosts []string // PAYMUX_JS_HOOKS_ALLOWED_HOSTS: hosts fetch() may call; "*.example.com" matches subdomains
}

// Load reads and validates configuration, reporting every problem at once.
func Load(lookupEnv func(string) (string, bool)) (Config, error) {
	get := func(k, def string) string {
		if v, ok := lookupEnv(k); ok && v != "" {
			return v
		}
		return def
	}
	var c Config
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	switch e := Env(get("PAYMUX_ENV", string(EnvProduction))); e {
	case EnvProduction, EnvDevelopment:
		c.Env = e
	default:
		bad("PAYMUX_ENV must be production or development")
	}

	if raw := get("PAYMUX_BASE_URL", ""); raw == "" {
		bad("PAYMUX_BASE_URL is required")
	} else if u, err := parseBaseURL(raw); err != nil {
		errs = append(errs, err)
	} else {
		c.BaseURL = u
	}

	c.Listen = get("PAYMUX_LISTEN", ":8080")
	c.InternalListen = get("PAYMUX_INTERNAL_LISTEN", "127.0.0.1:9100")
	c.DataDir = get("PAYMUX_DATA_DIR", "./data")

	c.DatabaseURL = get("PAYMUX_DATABASE_URL", "")
	switch {
	case c.DatabaseURL == "":
		c.DatabaseURL = "sqlite://" + filepath.ToSlash(filepath.Join(c.DataDir, "paymux.db"))
	case strings.HasPrefix(c.DatabaseURL, "sqlite://"), strings.HasPrefix(c.DatabaseURL, "postgres://"):
	default:
		bad("PAYMUX_DATABASE_URL must start with sqlite:// or postgres://")
	}

	key, err := loadMasterKey(lookupEnv)
	if err != nil {
		errs = append(errs, err)
	}
	c.MasterKey = key

	tz := get("PAYMUX_TZ", "Asia/Kolkata")
	if loc, err := time.LoadLocation(tz); err != nil {
		bad("PAYMUX_TZ %q is not a valid time zone", tz)
	} else {
		c.Location = loc
	}

	skip := false
	switch strings.ToLower(get("PAYMUX_DEV_SKIP_2FA", "false")) {
	case "true":
		skip = true
	case "false":
	default:
		bad("PAYMUX_DEV_SKIP_2FA must be true or false")
	}
	if skip && c.Env != "" && c.BaseURL != nil {
		if c.Env != EnvDevelopment || !isLocalHost(c.BaseURL.Hostname()) {
			bad("PAYMUX_DEV_SKIP_2FA is only allowed in development on a local host")
		} else {
			c.DevSkip2FA = true
		}
	}

	switch strings.ToLower(get("PAYMUX_ALLOW_PRIVATE_WEBHOOKS", "false")) {
	case "true":
		c.AllowPrivateWebhooks = true
	case "false":
	default:
		bad("PAYMUX_ALLOW_PRIVATE_WEBHOOKS must be true or false")
	}

	c.JSHooksDir = get("PAYMUX_JS_HOOKS_DIR", filepath.ToSlash(filepath.Join(c.DataDir, "hooks")))
	for _, h := range strings.Split(get("PAYMUX_JS_HOOKS_ALLOWED_HOSTS", ""), ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if !validHookHost(h) {
			bad("PAYMUX_JS_HOOKS_ALLOWED_HOSTS: %q is not a host name (use hooks.slack.com or *.example.com, no scheme, port or path)", h)
			continue
		}
		c.JSHooksAllowedHosts = append(c.JSHooksAllowedHosts, h)
	}

	c.SMTPHost = get("PAYMUX_SMTP_HOST", "")
	c.SMTPUsername = get("PAYMUX_SMTP_USERNAME", "")
	c.SMTPPassword = Secret(get("PAYMUX_SMTP_PASSWORD", ""))
	c.SMTPFrom = get("PAYMUX_SMTP_FROM", "")
	c.SMTPPort = 587
	if raw := get("PAYMUX_SMTP_PORT", ""); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 65535 {
			bad("PAYMUX_SMTP_PORT must be a port number (1-65535)")
		} else {
			c.SMTPPort = n
		}
	}
	if c.SMTPHost != "" && c.SMTPFrom == "" {
		bad("PAYMUX_SMTP_FROM is required when PAYMUX_SMTP_HOST is set")
	} else if c.SMTPFrom != "" {
		if _, err := mail.ParseAddress(c.SMTPFrom); err != nil {
			bad("PAYMUX_SMTP_FROM is not a valid email address")
		}
	}
	for _, raw := range strings.Split(get("PAYMUX_OPS_EMAILS", ""), ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		a, err := mail.ParseAddress(raw)
		if err != nil {
			bad("PAYMUX_OPS_EMAILS contains an invalid address %q", raw)
			continue
		}
		c.OpsEmails = append(c.OpsEmails, a.Address)
	}
	if raw := get("PAYMUX_OPS_WEBHOOK_URL", ""); raw != "" {
		if u, err := url.Parse(raw); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			bad("PAYMUX_OPS_WEBHOOK_URL must be an absolute http(s) URL")
		} else {
			c.OpsWebhookURL = raw
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

func isLocalHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || h == "127.0.0.1" ||
		strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".test")
}

func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("PAYMUX_BASE_URL must be an absolute http(s) URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLocalHost(u.Hostname()) {
			return nil, errors.New("PAYMUX_BASE_URL may use http:// only for localhost, 127.0.0.1, *.localhost or *.test")
		}
	default:
		return nil, errors.New("PAYMUX_BASE_URL must use https:// (or http:// for local hosts)")
	}
	return u, nil
}

// loadMasterKey reads exactly one of PAYMUX_MASTER_KEY or
// PAYMUX_MASTER_KEY_FILE. The value is standard base64 and must decode to 32
// bytes. Errors never include the key material.
func loadMasterKey(lookupEnv func(string) (string, bool)) ([]byte, error) {
	direct, hasDirect := lookupEnv("PAYMUX_MASTER_KEY")
	file, hasFile := lookupEnv("PAYMUX_MASTER_KEY_FILE")
	hasDirect = hasDirect && direct != ""
	hasFile = hasFile && file != ""
	if hasDirect == hasFile {
		return nil, errors.New("exactly one of PAYMUX_MASTER_KEY or PAYMUX_MASTER_KEY_FILE is required")
	}
	name, val := "PAYMUX_MASTER_KEY", direct
	if hasFile {
		name = "PAYMUX_MASTER_KEY_FILE"
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read file %q", name, file)
		}
		val = string(b)
	}
	val = strings.TrimSpace(val)
	key, ok := decodeKey(val)
	if !ok {
		return nil, fmt.Errorf("%s must be base64 encoded", name)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes, got %d", name, len(key))
	}
	return key, nil
}

func decodeKey(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// RedactedDatabaseURL returns the database URL with any password replaced by
// "xxxxx".
func (c Config) RedactedDatabaseURL() string {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "[unparseable database url]"
	}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}

// validHookHost accepts a bare host name or "*.suffix".
func validHookHost(h string) bool {
	name := strings.TrimPrefix(h, "*.")
	if name == "" || strings.ContainsAny(name, "/:@ *?#\\") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return false
		}
	}
	return true
}
