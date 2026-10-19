// Package remote is a connector that lives in another process. Every method is
// forwarded to an HTTP service as `POST {base_url}/{method}` with a JSON body,
// signed in both directions with Standard Webhooks (see pkg/connector/wire,
// which also has the server side). `GET {base_url}/info` supplies the Info:
// the remote decides which optional capabilities exist, and this connector
// implements exactly those interfaces.
//
// Account config: {"base_url": "https://connector.example.com/v1", "secret": "whsec_..."}.
// base_url must be https unless its host is loopback, and must not point at a
// loopback, private or link-local address (169.254.169.254, 10.0.0.0/8, ...):
// the host builds the HTTP client with a dial-time guard (Info.DenyPrivateNetwork)
// that checks every address a name resolves to and does not follow redirects.
// allow_insecure: true lifts both checks, for tests and local development.
//
// The registered Info is a placeholder: per-account facts such as
// merchant_of_record are only known after discovery. The host reads the live
// connector's Info when an account is created (it builds the connector, which
// is what probes GET /info and so also validates the URL and the secret) and
// records merchant_of_record on the account, which is where invoicing looks.
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
	"sync"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/wire"
)

const maxResponse = 8 << 20

var info = connector.Info{
	Name:        "remote",
	DisplayName: "Remote connector (HTTP)",
	Version:     "1",
	// base_url is operator-supplied, so the host guards the client (SSRF).
	DenyPrivateNetwork: true,
	ConfigSchema: json.RawMessage(`{"type":"object","required":["base_url","secret"],"properties":{
		"base_url":{"type":"string","title":"Base URL of the connector service","x-endpoint":true},
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
	// The host's client refuses private targets at dial time (names included);
	// a literal address is also refused here so the account is rejected early.
	if u, _ := url.Parse(cfg.BaseURL); !cfg.AllowInsecure && (isLoopback(u.Hostname()) || (net.ParseIP(u.Hostname()) != nil && connector.IsPrivateIP(net.ParseIP(u.Hostname())))) {
		return nil, fmt.Errorf("%w: base_url %s (set allow_insecure for local development)", connector.ErrPrivateAddress, u.Hostname())
	}
	signer, err := wire.NewSigner(cfg.Secret)
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if deps.HTTP == nil {
		return nil, errors.New("remote: no HTTP client")
	}
	// Never follow a redirect, whatever the host's client does: the target could
	// be an internal address the base_url checks never saw.
	hc := *deps.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	deps.HTTP = &hc
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
	// Every attempt (the host's client retries a GET after a 5xx) is signed
	// afresh: a retry that reused the first attempt's message id would be
	// refused by the service's replay cache. The response is bound to the id of
	// the attempt that produced it, which is the last one signed.
	var idMu sync.Mutex
	var requestID string
	sign := func(h http.Header) {
		id := r.signer.SignRequest(h, verb, method, body, r.deps.Now())
		idMu.Lock()
		requestID = id
		idMu.Unlock()
	}
	var reader io.Reader // nil for an empty body, so that the client may retry a GET
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(connector.WithAttemptHook(ctx, func(a *http.Request) { sign(a.Header) }), verb, r.base+"/"+method, reader)
	if err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	sign(req.Header) // valid as built, for clients that do not call the hook
	resp, err := r.deps.HTTP.Do(req)
	if errors.Is(err, connector.ErrPrivateAddress) {
		return fmt.Errorf("remote %s: %w", method, err) // permanent: the account points at an internal address
	}
	if err != nil {
		return fmt.Errorf("%w: remote %s: %v", connector.ErrTemporary, method, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("%w: remote %s: reading response: %v", connector.ErrTemporary, method, err)
	}
	idMu.Lock()
	answered := requestID
	idMu.Unlock()
	if err := r.signer.VerifyResponse(resp.Header, answered, data, r.deps.Now()); err != nil {
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
