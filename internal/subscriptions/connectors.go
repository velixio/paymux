package subscriptions

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// RuntimeConnectors builds connector instances from provider accounts: the
// stored config is opened with the master-key box, the registered factory is
// called with Deps whose URLs point at /return/{account}/{ref} and
// /hooks/{account}. Instances are cached per account for the process.
//
// TODO(spec): the core/integration lane owns the canonical connector runtime
// (including the encrypted ConnectorKV table); this is the minimal stand-in
// the subscription engine needs, with an in-memory KV, and should be replaced
// by it. The sealing context ("provider_account:<id>") is an assumption.
type RuntimeConnectors struct {
	DB      *store.DB
	Box     *secrets.Box
	BaseURL string
	Logger  *slog.Logger
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]connector.Connector
	kv    map[string]connector.KV
}

type accountURLs struct{ base, account string }

func (u accountURLs) ReturnURL(ref string) string {
	return u.base + "/return/" + u.account + "/" + url.PathEscape(ref)
}
func (u accountURLs) WebhookURL() string { return u.base + "/hooks/" + u.account }

// Get returns the connector of a provider account.
func (r *RuntimeConnectors) Get(ctx context.Context, id string) (connector.Connector, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[id]; ok {
		return c, nil
	}
	var pa store.ProviderAccount
	if err := r.DB.Read(ctx, func(q store.Querier) (err error) { pa, err = store.GetProviderAccount(ctx, q, id); return }); err != nil {
		return nil, fmt.Errorf("subscriptions: provider account %s: %w", id, err)
	}
	factory, _, ok := connector.Lookup(pa.Connector)
	if !ok {
		return nil, fmt.Errorf("subscriptions: connector %q is not built in", pa.Connector)
	}
	cfg, err := r.Box.Open(pa.ConfigEnc, []byte("provider_account:"+pa.ID))
	if err != nil {
		return nil, fmt.Errorf("subscriptions: opening config of %s: %w", id, err)
	}
	if r.kv == nil {
		r.kv, r.cache = map[string]connector.KV{}, map[string]connector.Connector{}
	}
	kv := connector.NewMemKV(r.Now)
	r.kv[id] = kv
	c, err := factory(cfg, connector.Deps{KV: kv, Clock: r.Now, Logger: r.Logger,
		URLs: accountURLs{base: strings.TrimRight(r.BaseURL, "/"), account: pa.ID}})
	if err != nil {
		return nil, err
	}
	r.cache[id] = c
	return c, nil
}
