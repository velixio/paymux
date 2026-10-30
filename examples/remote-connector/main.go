// Command remote-connector is a reference remote-connector service (spec 14,
// seam S2): it serves one connector to Paymux over the wire protocol of
// pkg/connector/wire, instead of Paymux loading the connector in-process.
//
// It wraps the built-in Razorpay connector, but any connector.Connector
// works: replace the New call in build with your own connector's constructor.
// Paymux side: create a provider account of connector "remote" with the
// config {"base_url": "https://this-service:8090", "secret": "<REMOTE_SECRET>",
// "checkout_hosts": {"script": ["https://checkout.razorpay.com"]}}
// (add "allow_insecure": true to use http://127.0.0.1:8090 in local development).
// checkout_hosts is required: Paymux sends the customer only to the hosts the
// account declares, so list the origins your connector's checkouts use.
//
// Configuration (environment):
//
//	REMOTE_SECRET      whsec_<base64> shared with the Paymux account (required)
//	RAZORPAY_CONFIG    the Razorpay account config JSON (required)
//	PAYMUX_BASE_URL    public base URL of Paymux, for return and webhook URLs (required)
//	PAYMUX_ACCOUNT_ID  id of the Paymux provider account using this service (required)
//	LISTEN             listen address (default 127.0.0.1:8090)
//
// Note: the return and webhook URLs belong to Paymux, so this example is told
// them through PAYMUX_BASE_URL and PAYMUX_ACCOUNT_ID. Its connector KV is in
// memory: a restart empties it, which a real service would not do.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/velixio/paymux/connectors/razorpay"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/wire"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	h, err := build(os.Getenv, log)
	if err != nil {
		log.Error("remote-connector", "err", err)
		os.Exit(1)
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("serving the Razorpay connector", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("remote-connector", "err", err)
		os.Exit(1)
	}
}

// urls are the addresses Paymux serves for one provider account.
type urls struct{ base, account string }

func (u urls) ReturnURL(ref string) string {
	return u.base + "/return/" + u.account + "/" + url.PathEscape(ref)
}
func (u urls) WebhookURL() string { return u.base + "/hooks/" + u.account }

// build constructs the wrapped connector and its wire handler from the environment.
func build(getenv func(string) string, log *slog.Logger) (http.Handler, error) {
	need := func(k string) (string, error) {
		if v := getenv(k); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s is required", k)
	}
	secret, err := need("REMOTE_SECRET")
	if err != nil {
		return nil, err
	}
	cfg, err := need("RAZORPAY_CONFIG")
	if err != nil {
		return nil, err
	}
	base, err := need("PAYMUX_BASE_URL")
	if err != nil {
		return nil, err
	}
	account, err := need("PAYMUX_ACCOUNT_ID")
	if err != nil {
		return nil, err
	}
	deps := connector.Deps{
		HTTP:   connector.NewHTTPClient(connector.HTTPOptions{Logger: log, MaxRetries: 2}),
		KV:     connector.NewMemKV(nil),
		Logger: log,
		URLs:   urls{base: strings.TrimRight(base, "/"), account: account},
	}
	inner, err := razorpay.New(json.RawMessage(cfg), deps)
	if err != nil {
		return nil, err
	}
	return wire.NewHandler(inner, secret, nil)
}
