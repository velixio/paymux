// Package connectortest is the conformance kit every connector must pass:
//
//	func TestConformance(t *testing.T) { connectortest.Run(t, New, fixtures) }
package connectortest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// StatusCase is one provider status the connector must map.
type StatusCase struct {
	// Raw is the provider's status string.
	Raw string
	// WantRaw is the raw status expected in the snapshot when it differs from
	// Raw (for example after the connector captured an authorized payment).
	WantRaw string
	// Want is the expected SDK status.
	Want connector.PaymentStatus
	// WantAmount is the expected snapshot amount in minor units.
	WantAmount int64
	// Handler is the fake provider serving FetchPayment(Ref) with this status.
	Handler http.Handler
}

// WebhookFixtures describe signed webhooks.
type WebhookFixtures struct {
	// Valid is a correctly signed webhook; it must verify and parse.
	Valid connector.WebhookRequest
	// WrongSecret is the same body signed with a different secret.
	WrongSecret connector.WebhookRequest
	// SignatureHeaders are removed one at a time to test "missing header".
	SignatureHeaders []string
	// Stale, when set, is a correctly signed webhook with an old timestamp
	// (only for schemes that sign a timestamp).
	Stale *connector.WebhookRequest
}

// ReturnFixtures describe the return flow; optional.
type ReturnFixtures struct {
	Valid    connector.ReturnRequest
	Tampered connector.ReturnRequest
	Want     connector.ReturnResult
}

// Fixtures is everything Run needs.
type Fixtures struct {
	// Config is the account config passed to the factory.
	Config json.RawMessage
	// Now is the fixed clock handed to the connector (default 2026-10-17 UTC).
	Now time.Time
	// Ref is the PaymentRef used with Statuses.
	Ref connector.PaymentRef
	// Statuses must cover every provider status the connector knows.
	Statuses []StatusCase
	Webhooks WebhookFixtures
	Return   *ReturnFixtures
	// Checkout is the request for CreateCheckout; CheckoutHandler is the fake
	// provider answering it. CheckNextAction adds connector-specific checks.
	Checkout        connector.CheckoutRequest
	CheckoutHandler http.Handler
	CheckNextAction func(t *testing.T, na connector.NextAction)
	// OAuth is required of a connector that declares CapOAuth.
	OAuth *OAuthFixtures
	// ConnectionOK and ConnectionRejected are the fake provider answering the
	// call of a connector's TestConnection (ConnectionTester) once with working
	// credentials and once with rejected ones. They are required of a connector
	// that implements ConnectionTester.
	ConnectionOK, ConnectionRejected http.Handler
}

// OAuthFixtures drive the OAuth checks: Handler is the fake provider answering
// the token exchange of Code with a refresh token.
type OAuthFixtures struct {
	RedirectURI string
	Code        string
	Handler     http.Handler
}

// URLs is a fixed connector.URLs for tests.
type URLs struct{ Base string }

// ReturnURL implements connector.URLs.
func (u URLs) ReturnURL(ref string) string { return u.Base + "/return/acc/" + url.PathEscape(ref) }

// WebhookURL implements connector.URLs.
func (u URLs) WebhookURL() string { return u.Base + "/hooks/acc" }

// RedirectClient returns an HTTP client that sends every request, whatever its
// host or scheme, to the test server.
func RedirectClient(srv *httptest.Server) *http.Client {
	target, _ := url.Parse(srv.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		c := r.Clone(r.Context())
		c.URL.Scheme, c.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(c)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// NewDeps returns Deps wired to the fake server (nil srv leaves HTTP unset).
func NewDeps(srv *httptest.Server, now time.Time) connector.Deps {
	d := connector.Deps{
		KV:     connector.NewMemKV(func() time.Time { return now }),
		Clock:  func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		URLs:   URLs{Base: "https://pay.example.com"},
	}
	if srv != nil {
		d.HTTP = RedirectClient(srv)
	}
	return d
}

// Run executes the conformance suite.
func Run(t *testing.T, factory connector.Factory, fx Fixtures) {
	t.Helper()
	if fx.Now.IsZero() {
		fx.Now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	}
	build := func(t *testing.T, h http.Handler) connector.Connector {
		t.Helper()
		var srv *httptest.Server
		if h != nil {
			srv = httptest.NewServer(h)
			t.Cleanup(srv.Close)
		}
		c, err := factory(fx.Config, NewDeps(srv, fx.Now))
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		return c
	}
	ctx := context.Background()

	t.Run("webhook", func(t *testing.T) {
		c := build(t, nil)
		w := fx.Webhooks
		if err := c.VerifyWebhook(ctx, clone(w.Valid)); err != nil {
			t.Fatalf("valid webhook rejected: %v", err)
		}
		ns, err := c.ParseWebhook(ctx, clone(w.Valid))
		if err != nil || len(ns) == 0 {
			t.Fatalf("valid webhook did not parse: %v %v", ns, err)
		}
		for _, n := range ns {
			if n.EventID == "" || n.EventType == "" || n.Kind == "" {
				t.Errorf("notification lacks id/type/kind: %+v", n)
			}
		}
		t.Run("tampered_body", func(t *testing.T) {
			r := clone(w.Valid)
			r.Body = append(r.Body, ' ')
			wantInvalidSignature(t, c.VerifyWebhook(ctx, r))
		})
		t.Run("wrong_secret", func(t *testing.T) {
			wantInvalidSignature(t, c.VerifyWebhook(ctx, clone(w.WrongSecret)))
		})
		for _, h := range w.SignatureHeaders {
			t.Run("missing_"+h, func(t *testing.T) {
				r := clone(w.Valid)
				for k := range r.Headers {
					if equalFold(k, h) {
						delete(r.Headers, k)
					}
				}
				wantInvalidSignature(t, c.VerifyWebhook(ctx, r))
			})
		}
		if w.Stale != nil {
			t.Run("stale_timestamp", func(t *testing.T) {
				wantInvalidSignature(t, c.VerifyWebhook(ctx, clone(*w.Stale)))
			})
		}
	})

	t.Run("return", func(t *testing.T) {
		if fx.Return == nil {
			t.Skip("no return fixtures")
		}
		c := build(t, nil)
		got, err := c.HandleReturn(ctx, fx.Return.Valid)
		if err != nil || got != fx.Return.Want {
			t.Fatalf("valid return: got %+v err %v want %+v", got, err, fx.Return.Want)
		}
		if _, err := c.HandleReturn(ctx, fx.Return.Tampered); err == nil {
			t.Fatal("tampered return accepted")
		}
	})

	t.Run("status_mapping_total", func(t *testing.T) {
		if len(fx.Statuses) == 0 {
			t.Fatal("no status fixtures")
		}
		for _, sc := range fx.Statuses {
			t.Run(sc.Raw, func(t *testing.T) {
				c := build(t, sc.Handler)
				snap, err := c.FetchPayment(ctx, fx.Ref)
				if err != nil {
					t.Fatal(err)
				}
				if !snap.Status.Valid() {
					t.Fatalf("unmapped status %q", snap.Status)
				}
				if snap.Status != sc.Want {
					t.Errorf("status = %q want %q", snap.Status, sc.Want)
				}
				wantRaw := sc.Raw
				if sc.WantRaw != "" {
					wantRaw = sc.WantRaw
				}
				if snap.RawStatus != wantRaw {
					t.Errorf("raw status = %q want %q", snap.RawStatus, wantRaw)
				}
				if snap.Amount != sc.WantAmount {
					t.Errorf("amount = %d want %d", snap.Amount, sc.WantAmount)
				}
				roundTripJSON(t, snap)
			})
		}
	})

	t.Run("money_roundtrip", func(t *testing.T) {
		info := build(t, nil).Info()
		if len(info.Currencies) == 0 {
			t.Fatal("Info.Currencies is empty")
		}
		for cur, min := range info.MinChargeMinor {
			if min <= 0 || !slices.Contains(info.Currencies, cur) {
				t.Errorf("Info.MinChargeMinor[%q] = %d must be a positive minimum of one of Info.Currencies", cur, min)
			}
		}
		for cur, min := range info.MinRefundMinor {
			if min <= 0 || !slices.Contains(info.Currencies, cur) {
				t.Errorf("Info.MinRefundMinor[%q] = %d must be a positive minimum of one of Info.Currencies", cur, min)
			}
		}
		for _, cur := range info.Currencies {
			if _, err := money.Exponent(cur); err != nil {
				t.Errorf("currency %q: %v", cur, err)
				continue
			}
			for _, minor := range []int64{0, 1, 99, 100, 12345, 100000000} {
				s, err := money.FormatDecimal(minor, cur)
				if err != nil {
					t.Fatal(err)
				}
				back, err := money.ParseDecimal(s, cur)
				if err != nil || back != minor {
					t.Errorf("%s %d -> %q -> %d (%v)", cur, minor, s, back, err)
				}
			}
		}
	})

	t.Run("info_matches_interfaces", func(t *testing.T) {
		c := build(t, nil)
		info := c.Info()
		if info.Name == "" || info.Version == "" || info.DisplayName == "" {
			t.Errorf("Info incomplete: %+v", info)
		}
		if !json.Valid(info.ConfigSchema) {
			t.Error("Info.ConfigSchema is not valid JSON")
		}
		impl := map[connector.Capability]bool{
			connector.CapRefund:              isA[connector.Refunder](c),
			connector.CapDisputeRead:         isA[connector.DisputeReader](c),
			connector.CapDisputeRespond:      isA[connector.DisputeResponder](c),
			connector.CapList:                isA[connector.Lister](c),
			connector.CapSettle:              isA[connector.Settler](c),
			connector.CapCancel:              isA[connector.Canceler](c),
			connector.CapSubscriptionManager: isA[connector.SubscriptionManager](c),
			connector.CapMandateManager:      isA[connector.MandateManager](c),
			connector.CapPayout:              isA[connector.Payouter](c),
			connector.CapOAuth:               isA[connector.OAuthConnector](c),
			connector.CapWebhookConfig:       isA[connector.WebhookConfigurator](c),
		}
		for cap, has := range impl {
			if has != info.Has(cap) {
				t.Errorf("capability %q: implemented=%v declared=%v", cap, has, info.Has(cap))
			}
		}
		for _, cap := range info.Capabilities {
			if _, known := impl[cap]; !known {
				t.Errorf("unknown capability %q declared", cap)
			}
		}
		if info.Has(connector.CapMandateManager) {
			t.Log("MandateManager: ChargeMandate must pass ChargeRequest.ProviderRef as idempotency token (verify in connector tests)")
		}
	})

	t.Run("connection_test", func(t *testing.T) {
		if !isA[connector.ConnectionTester](build(t, nil)) {
			t.Skip("no ConnectionTester")
		}
		if fx.ConnectionOK == nil || fx.ConnectionRejected == nil {
			t.Fatal("the connector implements ConnectionTester: Fixtures.ConnectionOK and ConnectionRejected are required")
		}
		if err := build(t, fx.ConnectionOK).(connector.ConnectionTester).TestConnection(ctx); err != nil {
			t.Errorf("working credentials: %v", err)
		}
		err := build(t, fx.ConnectionRejected).(connector.ConnectionTester).TestConnection(ctx)
		if !errors.Is(err, connector.ErrInvalidCredentials) {
			t.Errorf("rejected credentials: %v (want ErrInvalidCredentials)", err)
		}
	})

	t.Run("oauth", func(t *testing.T) {
		if !isA[connector.OAuthConnector](build(t, nil)) {
			t.Skip("not an OAuth connector")
		}
		if fx.OAuth == nil {
			t.Fatal("the connector declares OAuth: Fixtures.OAuth is required")
		}
		c := build(t, fx.OAuth.Handler).(connector.OAuthConnector)
		scopes := c.OAuthDefaultScopes(fx.Config)
		if len(scopes) == 0 {
			t.Error("OAuthDefaultScopes is empty")
		}
		raw, err := c.OAuthAuthorizeURL(fx.Config, fx.OAuth.RedirectURI, "state-123", scopes)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Fatalf("authorize URL %q is not an https URL: %v", raw, err)
		}
		if q := u.Query(); q.Get("state") != "state-123" || q.Get("redirect_uri") != fx.OAuth.RedirectURI {
			t.Errorf("authorize URL lacks the state or the redirect URI: %s", raw)
		}
		if _, err := c.OAuthAuthorizeURL(fx.Config, fx.OAuth.RedirectURI, "", scopes); err == nil {
			t.Error("an authorize URL without a state was built (the state is what makes the callback safe)")
		}
		if out, err := c.OAuthCallbackConfig(fx.Config, url.Values{}); err != nil || !json.Valid(out) {
			t.Errorf("OAuthCallbackConfig with nothing to adjust: %s %v", out, err)
		}
		res, err := c.OAuthExchange(ctx, fx.Config, fx.OAuth.RedirectURI, fx.OAuth.Code)
		if err != nil || res.RefreshToken == "" {
			t.Fatalf("exchange: %+v %v", res, err)
		}
		roundTripJSON(t, res)
		if _, err := c.OAuthExchange(ctx, fx.Config, fx.OAuth.RedirectURI, ""); err == nil {
			t.Error("an empty code was exchanged")
		}
	})

	t.Run("create_checkout", func(t *testing.T) {
		c := build(t, fx.CheckoutHandler)
		res, err := c.CreateCheckout(ctx, fx.Checkout)
		if err != nil {
			t.Fatal(err)
		}
		if !res.NextAction.Valid() {
			t.Fatalf("invalid NextAction: %+v", res.NextAction)
		}
		CheckAllowed(t, c, res.NextAction)
		if fx.CheckNextAction != nil {
			fx.CheckNextAction(t, res.NextAction)
		}
		roundTripJSON(t, res)
	})

	t.Run("sdk_types_json_roundtrip", func(t *testing.T) {
		for _, v := range SampleValues() {
			roundTripJSON(t, v)
			forbidKinds(t, reflect.TypeOf(v), map[reflect.Type]bool{})
		}
	})
}

func isA[T any](c connector.Connector) bool { _, ok := c.(T); return ok }

// wantInvalidSignature requires ErrInvalidSignature, and not ErrUnsigned:
// a present-but-wrong, missing or stale signature must be rejected with 401
// and never reach a fetch, while ErrUnsigned is reserved for event types the
// provider does not sign at all.
func wantInvalidSignature(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("webhook accepted, want ErrInvalidSignature")
	}
	if errors.Is(err, connector.ErrUnsigned) {
		t.Fatalf("err = %v: a bad signature must not be reported as ErrUnsigned (that path triggers a fetch)", err)
	}
	if !errors.Is(err, connector.ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

func clone(w connector.WebhookRequest) connector.WebhookRequest {
	out := w
	out.Body = append([]byte(nil), w.Body...)
	out.Headers = map[string][]string{}
	for k, v := range w.Headers {
		out.Headers[k] = append([]string(nil), v...)
	}
	return out
}

func equalFold(a, b string) bool {
	return http.CanonicalHeaderKey(a) == http.CanonicalHeaderKey(b)
}

// roundTripJSON checks v survives marshal/unmarshal unchanged.
func roundTripJSON(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%T: marshal: %v", v, err)
	}
	ptr := reflect.New(reflect.TypeOf(v))
	if err := json.Unmarshal(b, ptr.Interface()); err != nil {
		t.Fatalf("%T: unmarshal: %v", v, err)
	}
	if got := ptr.Elem().Interface(); !reflect.DeepEqual(got, v) {
		t.Errorf("%T: JSON round trip changed value\n got: %#v\nwant: %#v", v, got, v)
	}
}

// forbidKinds fails if a type contains funcs, channels or non-map interfaces.
func forbidKinds(t *testing.T, ty reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[ty] {
		return
	}
	seen[ty] = true
	switch ty.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		t.Errorf("SDK type contains %s", ty)
	case reflect.Interface:
		t.Errorf("SDK type contains interface %s outside a map value", ty)
	case reflect.Pointer, reflect.Slice, reflect.Array:
		forbidKinds(t, ty.Elem(), seen)
	case reflect.Map:
		if ty.Elem().Kind() != reflect.Interface {
			forbidKinds(t, ty.Elem(), seen)
		}
	case reflect.Struct:
		for i := 0; i < ty.NumField(); i++ {
			forbidKinds(t, ty.Field(i).Type, seen)
		}
	}
}

// CheckAllowed fails the test unless the hub would let the connector send the
// customer to na: the URL is on the hosts the connector declares (CheckoutHoster,
// else Info.CheckoutHosts). Run checks CreateCheckout with it; a connector test
// can check its subscription and mandate checkouts too.
func CheckAllowed(t *testing.T, c any, na connector.NextAction) {
	t.Helper()
	cc, ok := c.(connector.Connector)
	if !ok {
		t.Fatalf("%T is not a connector", c)
	}
	if err := na.Allowed(connector.HostsOf(cc)); err != nil {
		t.Errorf("the hub would refuse this checkout: %v", err)
	}
}
