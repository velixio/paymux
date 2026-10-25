// Package connectors is the connector runtime: it manages provider accounts
// (encrypted config, validated against the connector's schema) and builds,
// caches and serves live connector.Connector instances with their
// dependencies (HTTP client, encrypted KV, URLs).
package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// Options configures a Service.
type Options struct {
	DB      *store.DB
	Box     *secrets.Box
	BaseURL *url.URL // PAYMUX_BASE_URL
	Logger  *slog.Logger
	Now     func() time.Time
	// HTTPClient overrides the per-account client (tests point it at a fake
	// provider). Nil builds connector.NewHTTPClient per account.
	HTTPClient func(pa store.ProviderAccount, info connector.Info, secretValues []string) *http.Client
	// Observe receives every provider HTTP attempt of the default per-account
	// clients, with the connector's name (see ProviderMetrics).
	Observe func(name string, o connector.RequestObservation)
	// CallRecorder, when set, gives each default per-account client the recorder
	// that receives its failed calls (the error log in the database).
	CallRecorder func(pa store.ProviderAccount, info connector.Info) connector.CallRecorder
	// KnownSecrets receives the secret config values of an account, under the
	// owner "account:<id>", each time its connector is built or rebuilt, so a
	// redactor can replace them wherever they appear.
	KnownSecrets func(owner string, values []string)
}

// Service builds and manages connectors.
type Service struct {
	o       Options
	baseURL string
	mu      sync.Mutex
	cache   map[string]*cached
	// alerts and reported serve reportMismatch.
	alerts   Alerter
	reported map[string]string
}

type cached struct {
	fingerprint string
	live        Live
}

// Live is a built connector together with the account it serves.
type Live struct {
	Account   store.ProviderAccount
	Connector connector.Connector
}

// New builds a Service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o, baseURL: strings.TrimRight(o.BaseURL.String(), "/"), cache: map[string]*cached{}, reported: map[string]string{}}
}

func configAAD(id string) []byte { return secrets.AADProviderConfig(id) }

// WebhookURL is the inbound webhook URL of an account.
func (s *Service) WebhookURL(accountID string) string { return s.baseURL + "/hooks/" + accountID }

func invalid(format string, a ...any) error { return core.NewError(core.ErrInvalid, format, a...) }

// CreateAccountInput describes a new provider account.
type CreateAccountInput struct {
	Connector, Name, Mode string
	Config                json.RawMessage
	// Currencies narrows the connector's declared list (default: all of it).
	Currencies []string
	// Purposes defaults to one_time.
	Purposes            []string
	StatementDescriptor string
	ProviderGSTIN       string
	// AppID, when set, lets that app use the account.
	AppID string
}

var validPurposes = []string{"one_time", "subscription", "payout"}

func checkCurrencies(want []string, info connector.Info) error {
	for _, c := range want {
		if !slices.Contains(info.Currencies, c) {
			return invalid("%s does not support currency %s", info.Name, c)
		}
	}
	return nil
}

// CreateAccount validates the config against the connector's schema, builds a
// connector from it as a final check, stores the config sealed with the
// master key, and optionally links an app. The config never reaches the
// audit log.
func (s *Service) CreateAccount(ctx context.Context, actor audit.Actor, sc core.OrgScope, in CreateAccountInput) (store.ProviderAccount, error) {
	if sc.OrgID == "" {
		return store.ProviderAccount{}, invalid("an org scope is required")
	}
	factory, info, ok := connector.Lookup(in.Connector)
	if !ok {
		return store.ProviderAccount{}, invalid("unknown connector %q", in.Connector)
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 200 {
		return store.ProviderAccount{}, invalid("name is required (max 200 chars)")
	}
	if in.Mode != "test" && in.Mode != "live" {
		return store.ProviderAccount{}, invalid("mode must be test or live")
	}
	if info.TestOnly && in.Mode != "test" {
		return store.ProviderAccount{}, invalid("the %s connector can only be used in test mode", in.Connector)
	}
	if err := ValidateConfig(info.ConfigSchema, in.Config); err != nil {
		return store.ProviderAccount{}, invalid("config: %v", err)
	}
	if in.Mode == "live" && allowInsecure(in.Config) {
		return store.ProviderAccount{}, errAllowInsecureLive
	}
	if err := checkCredentialMode(in.Mode, in.Config); err != nil {
		return store.ProviderAccount{}, err
	}
	if len(in.Currencies) == 0 {
		in.Currencies = slices.Clone(info.Currencies)
	}
	if err := checkCurrencies(in.Currencies, info); err != nil {
		return store.ProviderAccount{}, err
	}
	if len(in.Purposes) == 0 {
		in.Purposes = []string{"one_time"}
	}
	for _, p := range in.Purposes {
		if !slices.Contains(validPurposes, p) {
			return store.ProviderAccount{}, invalid("unknown purpose %q", p)
		}
	}
	id := ids.New("pa")
	live, err := factory(in.Config, s.deps(store.ProviderAccount{ID: id}, info, in.Config))
	if err != nil {
		return store.ProviderAccount{}, invalid("config: %v", err)
	}
	sealed, err := s.o.Box.Seal(in.Config, configAAD(id))
	if err != nil {
		return store.ProviderAccount{}, err
	}
	pa := store.ProviderAccount{ID: id, OrgID: sc.OrgID, Mode: in.Mode, Connector: in.Connector, Name: in.Name, Status: "active",
		ConfigEnc: sealed, StatementDescriptor: in.StatementDescriptor, ProviderGSTIN: in.ProviderGSTIN,
		AcceptedCurrencies: in.Currencies, EnabledPurposes: in.Purposes, CreatedAt: s.o.Now().UTC(),
		// A remote connector learns this by discovery, which the registry entry cannot say.
		MerchantOfRecord: info.MerchantOfRecord || (live != nil && live.Info().MerchantOfRecord)}
	err = s.o.DB.Write(ctx, func(tx store.Tx) error {
		if in.AppID != "" {
			if _, err := store.GetAppInOrg(ctx, tx, sc.OrgID, in.AppID); err != nil {
				return notFound(err, "app")
			}
		}
		if err := store.InsertProviderAccount(ctx, tx, pa); err != nil {
			return err
		}
		if in.AppID != "" {
			if err := store.LinkAppProviderAccount(ctx, tx, in.AppID, pa.ID); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "provider_account.create", Target: pa.ID,
			After: map[string]any{"connector": pa.Connector, "name": pa.Name, "mode": pa.Mode, "currencies": pa.AcceptedCurrencies, "app_id": in.AppID}})
	})
	return pa, err
}

func notFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return core.NewError(core.ErrNotFound, "%s not found", what)
	}
	return err
}

// Account returns an account of the scope's org (config stays sealed).
func (s *Service) Account(ctx context.Context, sc core.OrgScope, id string) (store.ProviderAccount, error) {
	var pa store.ProviderAccount
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		pa, err = store.GetProviderAccount(ctx, q, id)
		return
	})
	if err != nil {
		return store.ProviderAccount{}, notFound(err, "provider account")
	}
	if sc.OrgID == "" || pa.OrgID != sc.OrgID {
		return store.ProviderAccount{}, core.NewError(core.ErrNotFound, "provider account not found")
	}
	return pa, nil
}

// UpdateAccountInput carries the fields to change; zero fields are kept.
type UpdateAccountInput struct {
	Name       *string
	Status     *string // active | disabled
	Config     json.RawMessage
	Currencies []string
	// Purposes replaces the enabled purposes (one_time, subscription).
	Purposes []string
}

// UpdateAccount changes an account of the scope's org. A new config is
// validated like on create. Cached instances are dropped (and instances in
// other processes notice the changed config on their next use).
func (s *Service) UpdateAccount(ctx context.Context, actor audit.Actor, sc core.OrgScope, id string, in UpdateAccountInput) error {
	pa, err := s.Account(ctx, sc, id)
	if err != nil {
		return err
	}
	factory, info, ok := connector.Lookup(pa.Connector)
	if !ok {
		return invalid("connector %q is not available in this build", pa.Connector)
	}
	changed := []string{}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len(n) > 200 {
			return invalid("name is required (max 200 chars)")
		}
		pa.Name = n
		changed = append(changed, "name")
	}
	if in.Status != nil {
		if *in.Status != "active" && *in.Status != "disabled" {
			return invalid("status must be active or disabled")
		}
		pa.Status = *in.Status
		changed = append(changed, "status")
	}
	if len(in.Currencies) > 0 {
		if err := checkCurrencies(in.Currencies, info); err != nil {
			return err
		}
		pa.AcceptedCurrencies = in.Currencies
		changed = append(changed, "currencies")
	}
	if len(in.Purposes) > 0 {
		for _, p := range in.Purposes {
			if !slices.Contains(validPurposes, p) {
				return invalid("unknown purpose %q", p)
			}
		}
		pa.EnabledPurposes = in.Purposes
		changed = append(changed, "purposes")
	}
	if in.Config != nil {
		if err := ValidateConfig(info.ConfigSchema, in.Config); err != nil {
			return invalid("config: %v", err)
		}
		if pa.Mode == "live" && allowInsecure(in.Config) {
			return errAllowInsecureLive
		}
		if err := checkCredentialMode(pa.Mode, in.Config); err != nil {
			return err
		}
		if err := s.checkEndpointChange(pa, info, in.Config); err != nil {
			return err
		}
		if _, err := factory(in.Config, s.deps(pa, info, in.Config)); err != nil {
			return invalid("config: %v", err)
		}
		if pa.ConfigEnc, err = s.o.Box.Seal(in.Config, configAAD(id)); err != nil {
			return err
		}
		changed = append(changed, "config")
	}
	err = s.o.DB.Write(ctx, func(tx store.Tx) error {
		if err := store.UpdateProviderAccount(ctx, tx, pa, s.o.Now().UTC()); err != nil {
			return notFound(err, "provider account")
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "provider_account.update", Target: id,
			After: map[string]any{"changed": changed}})
	})
	s.Invalidate(id)
	return err
}

// TestResult is the outcome of TestConnection.
type TestResult struct {
	OK      bool
	Message string
}

// testRef is the payment reference TestConnection asks the provider about.
// No provider has a payment with it.
const testRef = "paymux-connection-test"

// TestConnection checks an account's credentials by asking the provider for a
// payment that cannot exist. "Not found" proves the credentials were accepted;
// "invalid credentials" proves they were not; anything else (network, rate
// limit, provider outage) is reported as inconclusive rather than as a pass.
func (s *Service) TestConnection(ctx context.Context, sc core.OrgScope, accountID string) (TestResult, error) {
	if _, err := s.Account(ctx, sc, accountID); err != nil {
		return TestResult{}, err
	}
	live, err := s.Get(ctx, accountID)
	if err != nil {
		return TestResult{OK: false, Message: "could not build the connector: " + err.Error()}, nil
	}
	return probe(ctx, live.Connector), nil
}

// probe tests a connector's credentials: through ConnectionTester when it has
// one, else by asking FetchPayment about a payment that cannot exist.
func probe(ctx context.Context, c connector.Connector) TestResult {
	if t, ok := c.(connector.ConnectionTester); ok {
		err := t.TestConnection(ctx)
		switch {
		case err == nil:
			return TestResult{OK: true, Message: "The provider accepted the credentials."}
		case errors.Is(err, connector.ErrInvalidCredentials):
			// The connector's own words say what is wrong; show them.
			detail := strings.TrimPrefix(err.Error(), connector.ErrInvalidCredentials.Error()+": ")
			if detail == err.Error() || detail == "" {
				return TestResult{Message: "The provider rejected the credentials (invalid credentials)."}
			}
			return TestResult{Message: "The provider rejected the credentials: " + detail}
		default:
			return TestResult{Message: "Could not tell: " + err.Error()}
		}
	}
	_, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: testRef})
	switch {
	case err == nil || errors.Is(err, connector.ErrNotFound):
		return TestResult{OK: true, Message: "The provider accepted the credentials."}
	case errors.Is(err, connector.ErrInvalidCredentials):
		return TestResult{Message: "The provider rejected the credentials (invalid credentials)."}
	default:
		return TestResult{Message: "Could not tell: " + err.Error()}
	}
}

// LinkApp lets an app of the scope's org use an account of the same org.
func (s *Service) LinkApp(ctx context.Context, actor audit.Actor, sc core.OrgScope, accountID, appID string) error {
	if _, err := s.Account(ctx, sc, accountID); err != nil {
		return err
	}
	return s.o.DB.Write(ctx, func(tx store.Tx) error {
		if _, err := store.GetAppInOrg(ctx, tx, sc.OrgID, appID); err != nil {
			return notFound(err, "app")
		}
		if err := store.LinkAppProviderAccount(ctx, tx, appID, accountID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "provider_account.link", Target: accountID,
			After: map[string]string{"app_id": appID}})
	})
}

// LoadKnownSecrets hands the secret config values of every stored account to
// Options.KnownSecrets, without building any connector. Call it at start-up, so
// the redactor knows the secrets before the first use of an account. An account
// whose config cannot be read is skipped (it is reported when it is used).
func (s *Service) LoadKnownSecrets(ctx context.Context) error {
	if s.o.KnownSecrets == nil {
		return nil
	}
	var accounts []store.ProviderAccount
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { accounts, err = store.ListEveryProviderAccount(ctx, q); return }); err != nil {
		return err
	}
	for _, pa := range accounts {
		_, info, ok := connector.Lookup(pa.Connector)
		if !ok {
			continue
		}
		cfg, err := s.o.Box.Open(pa.ConfigEnc, configAAD(pa.ID))
		if err != nil {
			continue
		}
		s.o.KnownSecrets("account:"+pa.ID, secretValues(info.ConfigSchema, cfg))
	}
	return nil
}

// Invalidate drops the cached instance of an account.
func (s *Service) Invalidate(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, accountID)
}

func fingerprint(pa store.ProviderAccount) string {
	h := sha256.New()
	h.Write([]byte(pa.Mode + "|"))
	h.Write(pa.ConfigEnc)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Get returns the live connector of an account, building it on first use.
// The account row is re-read on every call and the instance rebuilt when its
// config changed, so changes made by other processes take effect too.
func (s *Service) Get(ctx context.Context, accountID string) (Live, error) {
	live, mismatch, err := s.get(ctx, accountID)
	if mismatch != nil {
		s.reportMismatch(ctx, mismatch.pa, mismatch.fp, err) // after the lock is released: it may send mail
	}
	return live, err
}

type mismatch struct {
	pa store.ProviderAccount
	fp string
}

func (s *Service) get(ctx context.Context, accountID string) (Live, *mismatch, error) {
	var pa store.ProviderAccount
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		pa, err = store.GetProviderAccount(ctx, q, accountID)
		return
	})
	if err != nil {
		return Live{}, nil, notFound(err, "provider account")
	}
	fp := fingerprint(pa)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[accountID]; ok && c.fingerprint == fp {
		c.live.Account = pa // status and currency changes apply without a rebuild
		return c.live, nil, nil
	}
	factory, info, ok := connector.Lookup(pa.Connector)
	if !ok {
		return Live{}, nil, fmt.Errorf("connectors: %q is not available in this build", pa.Connector)
	}
	if info.TestOnly && pa.Mode != "test" {
		return Live{}, nil, fmt.Errorf("connectors: %s is a %s-mode account, but the %s connector can only be used in test mode", pa.ID, pa.Mode, pa.Connector)
	}
	cfg, err := s.o.Box.Open(pa.ConfigEnc, configAAD(pa.ID))
	if err != nil {
		return Live{}, nil, fmt.Errorf("connectors: decrypting config of %s: %w", pa.ID, err)
	}
	if err := checkCredentialMode(pa.Mode, cfg); err != nil {
		// Never built, whatever the status: a live account must not run with
		// sandbox settings. The account is reported once (Get).
		return Live{}, &mismatch{pa: pa, fp: fp}, fmt.Errorf("connectors: %s (%s, %s mode) is refused: %w: %s", pa.ID, pa.Connector, pa.Mode, ErrSandboxOnLive,
			strings.TrimPrefix(err.Error(), "config: "))
	}
	if pa.Mode == "live" && allowInsecure(cfg) {
		// Created before live accounts were refused the opt-out: it is ignored,
		// for the host's guard (deps) and for the connector's own checks.
		cfg = withoutAllowInsecure(cfg)
	}
	conn, err := factory(cfg, s.deps(pa, info, cfg))
	if err != nil {
		return Live{}, nil, fmt.Errorf("connectors: building %s: %w", pa.ID, err)
	}
	live := Live{Account: pa, Connector: conn}
	s.cache[accountID] = &cached{fingerprint: fp, live: live}
	return live, nil, nil
}

func (s *Service) deps(pa store.ProviderAccount, info connector.Info, cfg json.RawMessage) connector.Deps {
	logger := s.o.Logger.With("provider_account", pa.ID, "connector", info.Name)
	secretValues := secretValues(info.ConfigSchema, cfg)
	if s.o.KnownSecrets != nil {
		s.o.KnownSecrets("account:"+pa.ID, secretValues)
	}
	var hc *http.Client
	if s.o.HTTPClient != nil {
		hc = s.o.HTTPClient(pa, info, secretValues)
	} else {
		opts := connector.HTTPOptions{Logger: logger, Secrets: secretValues, RateLimit: info.RateLimit,
			// A connector that calls an operator-supplied URL gets a client that
			// refuses internal addresses, unless the account opts out.
			DenyPrivate: info.DenyPrivateNetwork && !(allowInsecure(cfg) && pa.Mode != "live")}
		if s.o.Observe != nil {
			opts.Observer = func(o connector.RequestObservation) { s.o.Observe(info.Name, o) }
		}
		if s.o.CallRecorder != nil {
			opts.Recorder = s.o.CallRecorder(pa, info)
		}
		hc = connector.NewHTTPClient(opts)
	}
	return connector.Deps{HTTP: hc, KV: &kvStore{s: s, account: pa.ID}, Clock: s.o.Now, Logger: logger,
		URLs: &urls{s: s, account: pa.ID}}
}

// errAllowInsecureLive is the refusal of allow_insecure on a live account.
var errAllowInsecureLive = invalid("config: allow_insecure is not allowed for a live account: it switches off the guard against internal addresses " +
	"and is for local development; use a test-mode account for that")

// withoutAllowInsecure returns cfg with the allow_insecure opt-out removed.
func withoutAllowInsecure(cfg json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(cfg, &m) != nil {
		return cfg
	}
	delete(m, "allow_insecure")
	out, err := json.Marshal(m)
	if err != nil {
		return cfg
	}
	return out
}

// schemaFlagged returns the config properties of the schema that carry the
// boolean annotation flag ("x-secret", "x-endpoint").
func schemaFlagged(schema json.RawMessage, flag string) []string {
	var sch struct {
		Properties map[string]map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &sch) != nil {
		return nil
	}
	var out []string
	for name, prop := range sch.Properties {
		if string(prop[flag]) == "true" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// checkEndpointChange refuses a config that points an endpoint field (a property
// annotated "x-endpoint": the address the account's secrets are sent to) somewhere
// else while a secret field still holds its stored value. The dashboard keeps the
// stored secret when its field is left blank, so without this a changed address
// would silently receive the old secret. The caller must enter the secret again:
// a secret typed for the new address cannot be one that was stored for the old.
func (s *Service) checkEndpointChange(pa store.ProviderAccount, info connector.Info, newCfg json.RawMessage) error {
	endpoints := schemaFlagged(info.ConfigSchema, "x-endpoint")
	secretFields := schemaFlagged(info.ConfigSchema, "x-secret")
	if len(endpoints) == 0 || len(secretFields) == 0 || len(pa.ConfigEnc) == 0 {
		return nil
	}
	oldRaw, err := s.o.Box.Open(pa.ConfigEnc, configAAD(pa.ID))
	if err != nil {
		return fmt.Errorf("connectors: decrypting config of %s: %w", pa.ID, err)
	}
	var oldCfg, next map[string]any
	if json.Unmarshal(oldRaw, &oldCfg) != nil || json.Unmarshal(newCfg, &next) != nil {
		return nil
	}
	for _, ep := range endpoints {
		if fmt.Sprint(oldCfg[ep]) == fmt.Sprint(next[ep]) {
			continue
		}
		for _, sf := range secretFields {
			if v, ok := next[sf].(string); ok && v != "" && fmt.Sprint(oldCfg[sf]) == v {
				return invalid("config: %s changed, but %s is still the stored secret: enter the secret again so that it is not sent to the new address", ep, sf)
			}
		}
	}
	return nil
}

// allowInsecure reports whether the account config sets "allow_insecure": true,
// the explicit opt-out from the private-network guard (local development).
func allowInsecure(cfg json.RawMessage) bool {
	var c struct {
		AllowInsecure bool `json:"allow_insecure"`
	}
	return json.Unmarshal(cfg, &c) == nil && c.AllowInsecure
}

// checkCredentialMode refuses provider credentials that plainly belong to the
// other mode: a sandbox switch or a test key on a live account (a public test
// card would then "pay" a live payment), or a live key on a test account (a
// test-mode API key would then move real money). Only unambiguous signals are
// checked; a sandbox switch left off on a test account is allowed.
func checkCredentialMode(mode string, cfg json.RawMessage) error {
	var c struct {
		TestMode  bool   `json:"test_mode"`  // PayU: test.payu.in
		Sandbox   bool   `json:"sandbox"`    // Cashfree, Whop, Zoho
		SecretKey string `json:"secret_key"` // Stripe
		KeyID     string `json:"key_id"`     // Razorpay
		ClientID  string `json:"client_id"`  // Cashfree: test app ids start with TEST
		// Cashfree secrets carry their environment (cfsk_ma_test_..., cfsk_ma_prod_...).
		ClientSecret string `json:"client_secret"`
		Key          string `json:"key"` // PayU merchant key
		// Zoho: scopes granted by the sandbox server, and a checkout URL on it.
		OAuthScopes       string `json:"oauth_scopes"`
		HostedCheckoutURL string `json:"hosted_checkout_url"`
	}
	if json.Unmarshal(cfg, &c) != nil {
		return nil // ValidateConfig has judged the shape already
	}
	hasPrefix := func(v string, ps ...string) bool {
		for _, p := range ps {
			if strings.HasPrefix(v, p) {
				return true
			}
		}
		return false
	}
	switch mode {
	case "live":
		if c.TestMode || c.Sandbox || hasPrefix(c.SecretKey, "sk_test_", "rk_test_") || hasPrefix(c.KeyID, "rzp_test_") ||
			hasPrefix(c.ClientID, "TEST") || (hasPrefix(c.ClientSecret, "cfsk_") && strings.Contains(c.ClientSecret, "_test_")) ||
			payuPublicTestKeys[c.Key] || strings.Contains(c.OAuthScopes, "ZohoPaySandbox") ||
			strings.Contains(strings.ToLower(c.HostedCheckoutURL), "sandbox") {
			return invalid("config: these are sandbox credentials; a live account needs the provider's live credentials")
		}
	case "test":
		if hasPrefix(c.SecretKey, "sk_live_", "rk_live_") || hasPrefix(c.KeyID, "rzp_live_") {
			return invalid("config: these are live credentials; a test account needs the provider's test credentials")
		}
	}
	return nil
}

// payuPublicTestKeys are the merchant keys PayU publishes for its test
// environment; no live account has one.
var payuPublicTestKeys = map[string]bool{"gtKFFx": true, "JBZaLc": true}

// ErrSandboxOnLive is the refusal to use a provider account whose stored
// config does not belong to its mode: a live account with sandbox credentials
// (a public test card would "pay" a live payment), or a test account with live
// ones.
var ErrSandboxOnLive = errors.New("the stored config does not belong to the account's mode")

// Alerter raises ops alerts (*notify.Alerts).
type Alerter interface {
	Raise(ctx context.Context, al notify.Alert) error
}

// SetAlerter sets where the refusals of ErrSandboxOnLive are reported.
func (s *Service) SetAlerter(a Alerter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = a
}

// reportMismatch logs and alerts that an account's config does not belong to
// its mode, once per account and config: a hot path must not repeat it.
func (s *Service) reportMismatch(ctx context.Context, pa store.ProviderAccount, fp string, cause error) {
	s.mu.Lock()
	if s.reported[pa.ID] == fp {
		s.mu.Unlock()
		return
	}
	s.reported[pa.ID] = fp
	alerts := s.alerts
	s.mu.Unlock()
	s.o.Logger.Error("connectors: provider account refused: its config does not belong to its mode",
		"provider_account", pa.ID, "connector", pa.Connector, "mode", pa.Mode, "err", cause.Error())
	if alerts != nil {
		_ = alerts.Raise(ctx, notify.Alert{Severity: notify.SeverityCritical, Kind: "provider.sandbox_on_live",
			Message: fmt.Sprintf("Provider account %s (%s, %s mode) is refused and receives nothing: %s", pa.ID, pa.Connector, pa.Mode, cause.Error()),
			Refs:    map[string]string{"provider_account": pa.ID}})
	}
}

// DisableSandboxOnLive is run at startup: it disables every active account
// whose stored config does not belong to its mode, with an audit entry and an
// alert, so that nothing is routed to it. It returns how many it disabled.
// Get refuses such an account anyway, whatever its status.
func (s *Service) DisableSandboxOnLive(ctx context.Context) (int, error) {
	var accounts []store.ProviderAccount
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { accounts, err = store.ListEveryProviderAccount(ctx, q); return }); err != nil {
		return 0, err
	}
	n := 0
	for _, pa := range accounts {
		cfg, err := s.o.Box.Open(pa.ConfigEnc, configAAD(pa.ID))
		if err != nil {
			continue
		}
		cause := checkCredentialMode(pa.Mode, cfg)
		if cause == nil {
			continue
		}
		s.reportMismatch(ctx, pa, fingerprint(pa), cause)
		if pa.Status != "active" {
			continue
		}
		err = s.o.DB.Write(ctx, func(tx store.Tx) error {
			cur, err := store.GetProviderAccount(ctx, tx, pa.ID)
			if err != nil {
				return err
			}
			cur.Status = "disabled"
			if err := store.UpdateProviderAccount(ctx, tx, cur, s.o.Now()); err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{OrgID: pa.OrgID, Actor: audit.System, Action: "provider_account.disable_sandbox_on_live",
				Target: pa.ID, Before: map[string]any{"status": "active"}, After: map[string]any{"status": "disabled", "reason": cause.Error()}})
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// secretValues returns the string values of config keys marked "x-secret".
func secretValues(schema, cfg json.RawMessage) []string {
	var sch struct {
		Properties map[string]struct {
			Secret bool `json:"x-secret"`
		} `json:"properties"`
	}
	var vals map[string]any
	if json.Unmarshal(schema, &sch) != nil || json.Unmarshal(cfg, &vals) != nil {
		return nil
	}
	var out []string
	for k, p := range sch.Properties {
		if v, ok := vals[k].(string); ok && p.Secret && v != "" {
			out = append(out, v)
		}
	}
	return out
}

// urls implements connector.URLs for one account.
type urls struct {
	s       *Service
	account string
}

func (u *urls) WebhookURL() string { return u.s.WebhookURL(u.account) }

// ReturnURL points at the app's primary active custom domain when the
// attempt's app has one, else at the base URL.
func (u *urls) ReturnURL(providerRef string) string {
	base := u.s.baseURL
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = u.s.o.DB.Read(ctx, func(q store.Querier) error {
		appID, err := store.AppIDForAttemptRef(ctx, q, providerRef)
		if err != nil {
			return nil // not an attempt yet: base URL
		}
		if host, ok, err := store.PrimaryActiveHost(ctx, q, appID); err == nil && ok {
			base = "https://" + host
		}
		return nil
	})
	return base + "/return/" + u.account + "/" + url.PathEscape(providerRef)
}

// kvStore implements connector.KV on connector_kv, sealing values.
type kvStore struct {
	s       *Service
	account string
}

func (k *kvStore) aad(key string) []byte { return secrets.AADConnectorKV(k.account, key) }

func (k *kvStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var enc []byte
	var ok bool
	err := k.s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		enc, ok, err = store.ConnectorKVGet(ctx, q, k.account, key, k.s.o.Now())
		return
	})
	if err != nil || !ok {
		return nil, false, err
	}
	val, err := k.s.o.Box.Open(enc, k.aad(key))
	if err != nil {
		return nil, false, err
	}
	return val, true, nil
}

func (k *kvStore) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	enc, err := k.s.o.Box.Seal(val, k.aad(key))
	if err != nil {
		return err
	}
	var exp time.Time
	if ttl > 0 {
		exp = k.s.o.Now().Add(ttl)
	}
	return k.s.o.DB.Write(ctx, func(tx store.Tx) error { return store.ConnectorKVSet(ctx, tx, k.account, key, enc, exp) })
}

func (k *kvStore) Delete(ctx context.Context, key string) error {
	return k.s.o.DB.Write(ctx, func(tx store.Tx) error { return store.ConnectorKVDelete(ctx, tx, k.account, key) })
}

// ValidateConfig is a basic check of cfg against a JSON-schema-like document:
// cfg must be an object; "required" keys must be present and non-null;
// "additionalProperties": false rejects unknown keys; property "type"
// (string, boolean, integer, number, object, array) and string "minLength"
// are enforced. A nil schema accepts any object.
func ValidateConfig(schema, cfg json.RawMessage) error {
	var vals map[string]json.RawMessage
	if err := json.Unmarshal(cfg, &vals); err != nil || vals == nil {
		return errors.New("must be a JSON object")
	}
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	var sch struct {
		Required   []string `json:"required"`
		Additional *bool    `json:"additionalProperties"`
		Properties map[string]struct {
			Type      string `json:"type"`
			MinLength int    `json:"minLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &sch); err != nil {
		return fmt.Errorf("connector schema is invalid: %w", err)
	}
	for _, k := range sch.Required {
		if v, ok := vals[k]; !ok || string(bytes.TrimSpace(v)) == "null" {
			return fmt.Errorf("%q is required", k)
		}
	}
	for k, raw := range vals {
		p, known := sch.Properties[k]
		if !known {
			if sch.Additional != nil && !*sch.Additional {
				return fmt.Errorf("unknown key %q", k)
			}
			continue
		}
		if err := checkType(k, p.Type, p.MinLength, raw); err != nil {
			return err
		}
	}
	return nil
}

func checkType(key, typ string, minLen int, raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("%q is not valid JSON", key)
	}
	bad := func() error { return fmt.Errorf("%q must be a %s", key, typ) }
	switch typ {
	case "string":
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		if len(s) < minLen {
			return fmt.Errorf("%q must not be empty", key)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return bad()
		}
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return bad()
		}
		if _, err := n.Int64(); err != nil {
			return bad()
		}
	case "number":
		if _, ok := v.(json.Number); !ok {
			return bad()
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return bad()
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return bad()
		}
	}
	return nil
}

// Verifiers returns the inbound.VerifierFunc backed by each account's
// connector: Verify runs VerifyWebhook, maps connector.ErrUnsigned (an event type the
// provider never signs) to inbound.ErrUnsigned and ErrInvalidSignature to a
// hard failure (401), and takes the event id from ParseWebhook.
func (s *Service) Verifiers() inbound.VerifierFunc {
	return func(ctx context.Context, accountID string) (inbound.Verifier, error) {
		live, err := s.Get(ctx, accountID)
		if errors.Is(err, core.ErrNotFound) {
			return nil, inbound.ErrUnknownAccount
		}
		if err != nil {
			return nil, err
		}
		return &verifier{conn: live.Connector, now: s.o.Now}, nil
	}
}

type verifier struct {
	conn connector.Connector
	now  func() time.Time
}

func (v *verifier) Verify(headers http.Header, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := connector.WebhookRequest{Method: http.MethodPost, Headers: map[string][]string(headers), Body: body,
		ReceivedAt: v.now().UnixMilli()}
	verr := v.conn.VerifyWebhook(ctx, req)
	if errors.Is(verr, connector.ErrInvalidSignature) {
		// Missing, wrong or stale signature: reject (401). Nothing is parsed
		// and nothing is fetched on behalf of this request.
		return "", connector.ErrInvalidSignature
	}
	var eventID string
	if verr == nil || errors.Is(verr, connector.ErrUnsigned) {
		// A connector that can name the event from the request alone does so:
		// ParseWebhook may call the provider (to confirm an unsigned notice), and
		// an inbound delivery must never cost a provider call. The background job
		// parses the stored event.
		named := false
		if ider, ok := v.conn.(connector.WebhookEventIDer); ok {
			eventID, named = ider.WebhookEventID(req)
		}
		if !named {
			if ns, err := v.conn.ParseWebhook(ctx, req); err == nil {
				for _, n := range ns {
					if n.EventID != "" {
						eventID = n.EventID
						break
					}
				}
			}
		}
	}
	switch {
	case verr == nil:
		return eventID, nil
	case errors.Is(verr, connector.ErrUnsigned):
		return eventID, inbound.ErrUnsigned
	}
	return "", verr
}
