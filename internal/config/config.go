// Package config loads Paymux bootstrap configuration from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	// OldMasterKeys are earlier master keys, still used to open values sealed
	// under them while a key rotation is under way (PAYMUX_MASTER_KEY_OLD,
	// PAYMUX_MASTER_KEY_OLD_FILE).
	OldMasterKeys        [][]byte
	Location             *time.Location
	DevSkip2FA           bool
	AllowPrivateWebhooks bool // PAYMUX_ALLOW_PRIVATE_WEBHOOKS: outbound webhooks may target private IPs

	// SMTP for customer and ops email. An empty SMTPHost disables email.
	SMTPHost, SMTPUsername, SMTPFrom string
	SMTPPort                         int // default 587 (STARTTLS)
	SMTPPassword                     Secret

	// TrustedProxies are the reverse proxies whose X-Forwarded-For header is
	// believed (PAYMUX_TRUSTED_PROXIES, CIDRs or addresses; "none" for no
	// proxy). Unset, any address on the local network is believed
	// (localNetworks), so a reverse proxy on the LAN needs no setting.
	TrustedProxies []netip.Prefix

	OpsEmails     []string // PAYMUX_OPS_EMAILS, bare addresses
	OpsWebhookURL string   // PAYMUX_OPS_WEBHOOK_URL, Slack-compatible

	// OIDC single sign-on for the dashboard. Enabled when OIDCIssuer is set.
	OIDCIssuer         string   // PAYMUX_OIDC_ISSUER
	OIDCClientID       string   // PAYMUX_OIDC_CLIENT_ID
	OIDCClientSecret   Secret   // PAYMUX_OIDC_CLIENT_SECRET
	OIDCAllowedDomains []string // PAYMUX_OIDC_ALLOWED_DOMAINS: e-mail domains that may be created at first sign-in
	OIDCDefaultRole    string   // PAYMUX_OIDC_DEFAULT_ROLE for those users (default viewer; never admin)

	// DBLogLevel is the lowest level of application log records kept in the
	// database (PAYMUX_DB_LOG_LEVEL; default warn). Info and debug are stored
	// only when asked for here.
	DBLogLevel slog.Level
	// DBLogLevelExplicit is set when info or debug was asked for by name.
	DBLogLevelExplicit bool

	// OTLPLogsEndpoint, when set (PAYMUX_OTLP_LOGS_ENDPOINT), also ships the
	// stored, redacted records to an OpenTelemetry collector over OTLP/HTTP
	// JSON. The endpoint is operator-configured, so a private address is fine.
	// OTLPLogsHeaders (PAYMUX_OTLP_LOGS_HEADERS, "Name=value,Name=value") are
	// sent with every request.
	OTLPLogsEndpoint string
	OTLPLogsHeaders  map[string]Secret

	// JavaScript hooks (pkg/jsvm). They run only when JSHooksDir exists.
	JSHooksDir          string   // PAYMUX_JS_HOOKS_DIR, default <data dir>/hooks
	JSHooksAllowedHosts []string // PAYMUX_JS_HOOKS_ALLOWED_HOSTS: hosts fetch() may call; "*.example.com" matches subdomains
}

// OIDCEnabled reports whether single sign-on is configured.
func (c Config) OIDCEnabled() bool { return c.OIDCIssuer != "" }

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
	old, err := loadOldMasterKeys(lookupEnv, key)
	if err != nil {
		errs = append(errs, err)
	}
	c.OldMasterKeys = old

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
		if c.Env != EnvDevelopment {
			bad("PAYMUX_DEV_SKIP_2FA is only allowed in development (PAYMUX_ENV=development)")
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
	proxies := get("PAYMUX_TRUSTED_PROXIES", localNetworks)
	if strings.EqualFold(strings.TrimSpace(proxies), "none") {
		proxies = ""
	}
	for _, raw := range strings.Split(proxies, ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		pfx, err := parseProxy(raw)
		if err != nil {
			bad("PAYMUX_TRUSTED_PROXIES: %v", err)
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, pfx)
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

	loadOIDC(&c, get, bad)
	loadLogs(&c, get, bad)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

// loadLogs reads the database-log level and the optional OTLP export.
func loadLogs(c *Config, get func(k, def string) string, bad func(string, ...any)) {
	c.DBLogLevel = slog.LevelWarn
	switch lv := strings.ToLower(get("PAYMUX_DB_LOG_LEVEL", "warn")); lv {
	case "warn":
	case "error":
		c.DBLogLevel = slog.LevelError
	case "info":
		c.DBLogLevel, c.DBLogLevelExplicit = slog.LevelInfo, true
	case "debug":
		c.DBLogLevel, c.DBLogLevelExplicit = slog.LevelDebug, true
	default:
		bad("PAYMUX_DB_LOG_LEVEL must be warn, error, info or debug")
	}
	endpoint, headers := get("PAYMUX_OTLP_LOGS_ENDPOINT", ""), get("PAYMUX_OTLP_LOGS_HEADERS", "")
	if endpoint == "" {
		if headers != "" {
			bad("PAYMUX_OTLP_LOGS_HEADERS needs PAYMUX_OTLP_LOGS_ENDPOINT")
		}
		return
	}
	if u, err := url.Parse(endpoint); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		bad("PAYMUX_OTLP_LOGS_ENDPOINT must be an absolute http(s) URL")
	} else {
		c.OTLPLogsEndpoint = endpoint
	}
	for _, pair := range strings.Split(headers, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		name, val, ok := strings.Cut(pair, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			bad("PAYMUX_OTLP_LOGS_HEADERS must be Name=value pairs separated by commas")
			continue
		}
		if c.OTLPLogsHeaders == nil {
			c.OTLPLogsHeaders = map[string]Secret{}
		}
		c.OTLPLogsHeaders[name] = Secret(strings.TrimSpace(val))
	}
}

var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

// loadOIDC reads the PAYMUX_OIDC_* settings: all of issuer, client id and
// client secret or none.
func loadOIDC(c *Config, get func(k, def string) string, bad func(string, ...any)) {
	c.OIDCIssuer = strings.TrimRight(get("PAYMUX_OIDC_ISSUER", ""), "/")
	c.OIDCClientID = get("PAYMUX_OIDC_CLIENT_ID", "")
	c.OIDCClientSecret = Secret(get("PAYMUX_OIDC_CLIENT_SECRET", ""))
	domains, role := get("PAYMUX_OIDC_ALLOWED_DOMAINS", ""), get("PAYMUX_OIDC_DEFAULT_ROLE", "")
	if c.OIDCIssuer == "" && c.OIDCClientID == "" && c.OIDCClientSecret == "" && domains == "" && role == "" {
		return
	}
	if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" {
		bad("PAYMUX_OIDC_ISSUER, PAYMUX_OIDC_CLIENT_ID and PAYMUX_OIDC_CLIENT_SECRET must all be set to enable single sign-on")
		return
	}
	if u, err := url.Parse(c.OIDCIssuer); err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname()))) {
		bad("PAYMUX_OIDC_ISSUER must be an https URL (http only for localhost)")
	}
	for _, d := range strings.Split(domains, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d == "" {
			continue
		}
		if !domainRE.MatchString(d) {
			bad("PAYMUX_OIDC_ALLOWED_DOMAINS contains %q, which is not a domain such as example.com", d)
			continue
		}
		c.OIDCAllowedDomains = append(c.OIDCAllowedDomains, d)
	}
	c.OIDCDefaultRole = role
	switch {
	case role == "":
		c.OIDCDefaultRole = "viewer"
	case role == "admin":
		bad("PAYMUX_OIDC_DEFAULT_ROLE must not be admin: people provisioned by sign-in never get full access automatically")
	}
}

// parseProxy reads a CIDR block or a single address. A /0 block would trust
// every client, which defeats the setting, so it is refused.
// localNetworks is the default PAYMUX_TRUSTED_PROXIES: loopback, the private
// (RFC 1918 / RFC 4193) and the link-local ranges. A host on these networks
// can set the client address the hub believes (login lockout, audit), which
// is the trade-off of not configuring the proxy's exact address.
const localNetworks = "127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,::1/128,fc00::/7,fe80::/10"

func parseProxy(raw string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(raw, "/") {
		var err error
		if p, err = netip.ParsePrefix(raw); err != nil {
			return p, fmt.Errorf("%q is not a CIDR block", raw)
		}
	} else {
		a, err := netip.ParseAddr(raw)
		if err != nil {
			return p, fmt.Errorf("%q is not an IP address or CIDR block", raw)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Bits() == 0 {
		return p, fmt.Errorf("%q would trust every client", raw)
	}
	return p, nil
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

// loadOldMasterKeys reads the earlier keys of a rotation from
// PAYMUX_MASTER_KEY_OLD (comma separated) and PAYMUX_MASTER_KEY_OLD_FILE (a
// file with keys separated by commas, spaces or new lines), both base64. The
// active key among them is ignored, and so is a key given twice.
func loadOldMasterKeys(lookupEnv func(string) (string, bool), active []byte) ([][]byte, error) {
	var parts []string
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
	}
	if v, ok := lookupEnv("PAYMUX_MASTER_KEY_OLD"); ok {
		parts = append(parts, split(v)...)
	}
	if f, ok := lookupEnv("PAYMUX_MASTER_KEY_OLD_FILE"); ok && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("PAYMUX_MASTER_KEY_OLD_FILE: cannot read file %q", f)
		}
		parts = append(parts, split(string(b))...)
	}
	var out [][]byte
	seen := map[string]bool{string(active): true}
	for i, p := range parts {
		key, ok := decodeKey(p)
		if !ok {
			return nil, fmt.Errorf("PAYMUX_MASTER_KEY_OLD: key %d is not base64 encoded", i+1)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("PAYMUX_MASTER_KEY_OLD: key %d must decode to 32 bytes, got %d", i+1, len(key))
		}
		if !seen[string(key)] {
			seen[string(key)] = true
			out = append(out, key)
		}
	}
	return out, nil
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

// IsLocalHost reports whether h names this machine (localhost or a loopback
// address).
func IsLocalHost(h string) bool { return isLocalHost(h) }

// DatabaseTLSWarning returns a message when databaseURL is a PostgreSQL URL
// whose server is not on this machine and whose sslmode does not verify the
// server's certificate and name: sslmode missing (then PGSSLMODE, which the
// driver reads, decides), disable, allow, prefer or require. All of those can
// be talked into plain text or onto a forged server by anyone on the path.
// verify-ca and verify-full are accepted. The message carries no credentials.
func DatabaseTLSWarning(databaseURL string, getenv func(string) string) string {
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return ""
	}
	u, err := url.Parse(databaseURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	host := u.Hostname()
	if host == "" {
		host = q.Get("host") // a socket directory, or a host given as a parameter
	}
	if host == "" || strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback() {
		return "" // this machine: a socket directory, localhost or a loopback address
	}
	mode := strings.ToLower(q.Get("sslmode"))
	if mode == "" && getenv != nil {
		mode = strings.ToLower(getenv("PGSSLMODE"))
	}
	if mode == "verify-full" || mode == "verify-ca" {
		return ""
	}
	if mode == "" {
		mode = "unset (the driver then tries TLS but accepts plain text)"
	}
	return "the PostgreSQL connection to " + host + " does not verify the server's certificate (sslmode " + mode +
		"); use sslmode=verify-full in PAYMUX_DATABASE_URL, with the CA in sslrootcert if it is a private one"
}
