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
	return &Service{o: o, baseURL: strings.TrimRight(o.BaseURL.String(), "/"), cache: map[string]*cached{}}
}

func configAAD(id string) []byte { return []byte("provider_accounts.config_enc:" + id) }

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

// Invalidate drops the cached instance of an account.
func (s *Service) Invalidate(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, accountID)
}

func fingerprint(pa store.ProviderAccount) string {
	h := sha256.New()
	h.Write(pa.ConfigEnc)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Get returns the live connector of an account, building it on first use.
// The account row is re-read on every call and the instance rebuilt when its
// config changed, so changes made by other processes take effect too.
func (s *Service) Get(ctx context.Context, accountID string) (Live, error) {
	var pa store.ProviderAccount
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		pa, err = store.GetProviderAccount(ctx, q, accountID)
		return
	})
	if err != nil {
		return Live{}, notFound(err, "provider account")
	}
	fp := fingerprint(pa)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[accountID]; ok && c.fingerprint == fp {
		c.live.Account = pa // status and currency changes apply without a rebuild
		return c.live, nil
	}
	factory, info, ok := connector.Lookup(pa.Connector)
	if !ok {
		return Live{}, fmt.Errorf("connectors: %q is not available in this build", pa.Connector)
	}
	if info.TestOnly && pa.Mode != "test" {
		return Live{}, fmt.Errorf("connectors: %s is a %s-mode account, but the %s connector can only be used in test mode", pa.ID, pa.Mode, pa.Connector)
	}
	cfg, err := s.o.Box.Open(pa.ConfigEnc, configAAD(pa.ID))
	if err != nil {
		return Live{}, fmt.Errorf("connectors: decrypting config of %s: %w", pa.ID, err)
	}
	conn, err := factory(cfg, s.deps(pa, info, cfg))
	if err != nil {
		return Live{}, fmt.Errorf("connectors: building %s: %w", pa.ID, err)
	}
	live := Live{Account: pa, Connector: conn}
	s.cache[accountID] = &cached{fingerprint: fp, live: live}
	return live, nil
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
			DenyPrivate: info.DenyPrivateNetwork && !allowInsecure(cfg)}
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

// allowInsecure reports whether the account config sets "allow_insecure": true,
// the explicit opt-out from the private-network guard (local development).
func allowInsecure(cfg json.RawMessage) bool {
	var c struct {
		AllowInsecure bool `json:"allow_insecure"`
	}
	return json.Unmarshal(cfg, &c) == nil && c.AllowInsecure
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

func (k *kvStore) aad(key string) []byte { return []byte("connector_kv:" + k.account + ":" + key) }

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
		if ns, err := v.conn.ParseWebhook(ctx, req); err == nil {
			for _, n := range ns {
				if n.EventID != "" {
					eventID = n.EventID
					break
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
