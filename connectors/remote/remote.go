// Package remote is a connector that lives in another process. Every method is
// forwarded to an HTTP service as `POST {base_url}/{method}` with a JSON body,
// signed in both directions with Standard Webhooks (see pkg/connector/wire,
// which also has the server side). `GET {base_url}/info` supplies the Info:
// the remote decides which optional capabilities exist, and this connector
// implements exactly those interfaces.
//
// Account config: {"base_url": "https://connector.example.com/v1", "secret": "whsec_..."}.
// base_url must be https unless its host is loopback (allow_insecure: true
// lifts that, for tests).
//
// TODO(spec): the registered Info is a placeholder; per-account facts such as
// merchant_of_record are only known after discovery, so anything that reads the
// registry instead of the live connector (invoice suppression for
// merchant-of-record connectors) cannot see them yet.
// TODO(spec): the account's secret and URL are not validated against the
// remote until the first call; account creation could probe GET /info.
package remote

//go:generate go run gen.go

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/wire"
)

const maxResponse = 8 << 20

var info = connector.Info{
	Name:        "remote",
	DisplayName: "Remote connector (HTTP)",
	Version:     "1",
	ConfigSchema: json.RawMessage(`{"type":"object","required":["base_url","secret"],"properties":{
		"base_url":{"type":"string","title":"Base URL of the connector service"},
		"secret":{"type":"string","title":"Signing secret (whsec_...)","x-secret":true}}}`),
}

func init() { connector.Register("remote", New, info) }

type config struct {
	BaseURL string `json:"base_url"`
	Secret  string `json:"secret"`
	// AllowInsecure permits a plain-http base_url on a non-loopback host.
	// For tests only: the signature authenticates messages but does not
	// encrypt them, and customer data travels in the bodies.
	AllowInsecure bool `json:"allow_insecure,omitempty"`
}

// checkBaseURL requires https, except for a loopback host (a service on the
// same machine, or a test) or when allowInsecure is set.
func checkBaseURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("remote: base_url must be an https URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopback(u.Hostname()) {
			return nil
		}
		return errors.New("remote: base_url must use https (http is only accepted for a loopback host)")
	}
	return errors.New("remote: base_url must be an https URL")
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type remote struct {
	base   string
	signer *wire.Signer
	deps   connector.Deps
	info   connector.Info
}

// New discovers the remote's Info and returns a connector with exactly the
// optional interfaces it declares.
func New(raw json.RawMessage, deps connector.Deps) (connector.Connector, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("remote: bad config: %w", err)
	}
	if err := checkBaseURL(cfg.BaseURL, cfg.AllowInsecure); err != nil {
		return nil, err
	}
	signer, err := wire.NewSigner(cfg.Secret)
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if deps.HTTP == nil {
		return nil, errors.New("remote: no HTTP client")
	}
	r := &remote{base: strings.TrimRight(cfg.BaseURL, "/"), signer: signer, deps: deps}
	var ri connector.Info
	if err := r.call(context.Background(), wire.MethodInfo, nil, &ri); err != nil {
		return nil, fmt.Errorf("remote: discovery: %w", err)
	}
	ri.Name = "remote"
	r.info = ri
	mask := 0
	for _, c := range ri.Capabilities {
		mask |= capabilityBits[c] // unknown capabilities add nothing
	}
	return compose(r, mask), nil
}

// call performs one signed exchange. method info is a GET with no body.
func (r *remote) call(ctx context.Context, method string, in, out any) error {
	verb, body := http.MethodPost, []byte(nil)
	if method == wire.MethodInfo {
		verb = http.MethodGet
	} else {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("remote: encoding %s: %w", method, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, verb, r.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	requestID := r.signer.SignRequest(req.Header, verb, method, body, r.deps.Now())
	resp, err := r.deps.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: remote %s: %v", connector.ErrTemporary, method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("%w: remote %s: reading response: %v", connector.ErrTemporary, method, err)
	}
	if err := r.signer.VerifyResponse(resp.Header, requestID, data, r.deps.Now()); err != nil {
		if resp.Header.Get(wire.HeaderSignature) == "" && resp.StatusCode >= 500 {
			return fmt.Errorf("%w: remote %s: HTTP %d", connector.ErrTemporary, method, resp.StatusCode) // a proxy or crash, not the service
		}
		return fmt.Errorf("%w: remote %s: response %v (wrong secret?)", connector.ErrInvalidCredentials, method, err)
	}
	if resp.StatusCode != http.StatusOK {
		var env wire.Envelope
		if json.Unmarshal(data, &env) != nil || env.Error.Code == "" {
			return fmt.Errorf("%w: remote %s: HTTP %d", connector.ErrTemporary, method, resp.StatusCode)
		}
		return env.Error.Err()
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: remote %s: undecodable result: %v", connector.ErrTemporary, method, err)
	}
	return nil
}

// rpc is call with a typed result.
func rpc[Out any](ctx context.Context, r *remote, method string, in any) (Out, error) {
	var out Out
	err := r.call(ctx, method, in, &out)
	return out, err
}

func (r *remote) Info() connector.Info { return r.info }

func (r *remote) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return rpc[connector.CheckoutResult](ctx, r, wire.MethodCreateCheckout, req)
}

func (r *remote) HandleReturn(ctx context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	return rpc[connector.ReturnResult](ctx, r, wire.MethodHandleReturn, req)
}

func (r *remote) VerifyWebhook(ctx context.Context, req connector.WebhookRequest) error {
	_, err := rpc[wire.Empty](ctx, r, wire.MethodVerifyWebhook, req)
	return err
}

func (r *remote) ParseWebhook(ctx context.Context, req connector.WebhookRequest) ([]connector.Notification, error) {
	out, err := rpc[wire.Notifications](ctx, r, wire.MethodParseWebhook, req)
	return out.Items, err
}

func (r *remote) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	return rpc[connector.PaymentSnapshot](ctx, r, wire.MethodFetchPayment, ref)
}
