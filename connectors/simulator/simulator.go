// Package simulator is a payment provider that exists only on this machine,
// for test mode: it lets a developer, or an app under development, exercise
// the whole Paymux flow (checkout, return, signed webhooks, refunds,
// disputes, subscriptions and mandates, settlements) without any provider
// account or key.
//
// The "provider" keeps its state in the account's key/value store. The
// customer's side is a hosted page, served by Paymux at /sim/{ref}, whose
// buttons call Controller.Decide; the provider's side (webhooks) is the
// connector POSTing signed events to Paymux's own /hooks/{account} URL, so the
// normal inbound pipeline, with signature checks and fetch-the-truth, is what
// processes them. Nothing here is special-cased inside Paymux except that the
// connector is TestOnly: it cannot be linked to a live-mode account.
//
// It imports only the standard library and pkg/....
package simulator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Name is the connector's registry name.
const Name = "simulator"

const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "refund_delay_seconds": {"type": "integer", "title": "Refund delay (seconds)", "minimum": 0, "maximum": 3600, "default": 0,
      "description": "0 makes refunds succeed at once; more makes them pending and then succeeded after this long."},
    "payout_delay_seconds": {"type": "integer", "title": "Payout delay (seconds)", "minimum": 0, "maximum": 3600, "default": 0,
      "description": "0 pays payouts at once; more keeps them processing for this long (or until you end them from the dashboard)."},
    "webhook_secret": {"type": "string", "title": "Webhook secret", "x-secret": true,
      "description": "Optional. By default the simulator makes up a secret and keeps it; set one only to make webhooks reproducible."}
  }
}`

var info = connector.Info{
	Version:      "0.1.0",
	DisplayName:  "Simulator (test mode only)",
	ConfigSchema: json.RawMessage(configSchema),
	Capabilities: []connector.Capability{connector.CapRefund, connector.CapDisputeRead, connector.CapDisputeRespond, connector.CapList,
		connector.CapSettle, connector.CapCancel, connector.CapSubscriptionManager, connector.CapMandateManager, connector.CapPayout},
	Currencies:         []string{"INR", "USD", "BDT"},
	Methods:            []string{"simulated", "upi", "card"},
	MerchantOfRecord:   false,
	SubscriptionModes:  []connector.BillingMode{connector.ProviderManaged, connector.MerchantTriggered},
	PreDebitPerAttempt: false,
	MandateLimits:      []connector.MandateLimit{{Method: "upi", Currency: "INR", MaxAmount: 1500000}},
	MandateSetupAmount: 100,
	// The registration debit is not reversed by the "provider".
	RefundsVerificationDebit: false,
	RateLimit:                connector.RateLimit{RequestsPerSecond: 100, Burst: 100},
	TestOnly:                 true,
}

func init() { connector.Register(Name, New, info) }

type config struct {
	RefundDelaySeconds int    `json:"refund_delay_seconds"`
	PayoutDelaySeconds int    `json:"payout_delay_seconds"`
	WebhookSecret      string `json:"webhook_secret"`
}

// mu serialises every read-modify-write of the simulated provider's state. The
// state lives in the account's KV, shared by every connector instance of the
// process, so the lock is global.
var mu sync.Mutex

type conn struct {
	cfg  config
	deps connector.Deps
	log  *slog.Logger
}

// New builds the connector.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("simulator: config: %w", err)
		}
	}
	if cfg.RefundDelaySeconds < 0 || cfg.RefundDelaySeconds > 3600 {
		return nil, errors.New("simulator: refund_delay_seconds must be 0 to 3600")
	}
	if cfg.PayoutDelaySeconds < 0 || cfg.PayoutDelaySeconds > 3600 {
		return nil, errors.New("simulator: payout_delay_seconds must be 0 to 3600")
	}
	if deps.KV == nil {
		return nil, errors.New("simulator: needs the connector key/value store")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	return &conn{cfg: cfg, deps: deps, log: log}, nil
}

func (c *conn) Info() connector.Info { i := info; i.Name = Name; return i }

// ---- state helpers ----

func (c *conn) now() time.Time { return c.deps.Now() }
func (c *conn) ms() int64      { return c.now().UnixMilli() }

func (c *conn) load(ctx context.Context, key string, v any) (bool, error) {
	b, ok, err := c.deps.KV.Get(ctx, key)
	if err != nil || !ok {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (c *conn) save(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.deps.KV.Set(ctx, key, b, 0)
}

func (c *conn) loadString(ctx context.Context, key string) (string, bool, error) {
	b, ok, err := c.deps.KV.Get(ctx, key)
	return string(b), ok, err
}

func (c *conn) saveString(ctx context.Context, key, val string) error {
	return c.deps.KV.Set(ctx, key, []byte(val), 0)
}

// append adds id to the list stored under key (the lists back ListPayments and
// friends, which the KV cannot enumerate).
func (c *conn) appendList(ctx context.Context, key, id string) error {
	var ids []string
	if _, err := c.load(ctx, key, &ids); err != nil {
		return err
	}
	return c.save(ctx, key, append(ids, id))
}

func (c *conn) list(ctx context.Context, key string) ([]string, error) {
	var ids []string
	_, err := c.load(ctx, key, &ids)
	return ids, err
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// secret is the account's webhook signing secret, created on first use.
func (c *conn) secret(ctx context.Context) (string, error) {
	if c.cfg.WebhookSecret != "" {
		return c.cfg.WebhookSecret, nil
	}
	mu.Lock()
	defer mu.Unlock()
	if s, ok, err := c.loadString(ctx, "secret"); err != nil || ok {
		return s, err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	s := "simsec_" + hex.EncodeToString(b[:])
	return s, c.saveString(ctx, "secret", s)
}

// baseURL is the origin of this Paymux (from the webhook URL it gave us), and
// accountID the provider account (the last element of that URL).
func (c *conn) origin() (base, accountID string, err error) {
	if c.deps.URLs == nil {
		return "", "", errors.New("simulator: no URLs configured")
	}
	u, err := url.Parse(c.deps.URLs.WebhookURL())
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("simulator: bad webhook URL %q", c.deps.URLs.WebhookURL())
	}
	return u.Scheme + "://" + u.Host, strings.TrimPrefix(u.Path[strings.LastIndex(u.Path, "/"):], "/"), nil
}

// pageOrigin is the origin of the hub's /sim page: the checkout host when
// PAYMUX_CHECKOUT_URL is set, else the base host. acc is the account id.
func (c *conn) pageOrigin() (origin, acc string, err error) {
	base, acc, err := c.origin()
	if err != nil {
		return "", "", err
	}
	if co, ok := c.deps.URLs.(interface{ CheckoutOrigin() string }); ok && co.CheckoutOrigin() != "" {
		base = co.CheckoutOrigin()
	}
	return base, acc, nil
}

// CheckoutHosts: the simulator only sends the buyer to the hub's own /sim page,
// whose origin (http in development) is Self.
func (c *conn) CheckoutHosts() connector.CheckoutHosts {
	base, _, err := c.pageOrigin()
	if err != nil {
		return connector.CheckoutHosts{}
	}
	return connector.CheckoutHosts{Self: base}
}

// pageURL is the hosted page for a checkout reference.
func (c *conn) pageURL(ref string) (string, error) {
	base, acc, err := c.pageOrigin()
	if err != nil {
		return "", err
	}
	return base + "/sim/" + url.PathEscape(ref) + "?pa=" + url.QueryEscape(acc), nil
}
