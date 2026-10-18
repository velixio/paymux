package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// accountsFake is Zoho's accounts server: it records the token requests and
// answers them as a script says.
type accountsFake struct {
	mu     sync.Mutex
	hosts  []string
	forms  []url.Values
	status int
	body   map[string]any
}

func (a *accountsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(b))
	a.mu.Lock()
	a.hosts = append(a.hosts, r.Header.Get("X-Original-Host"))
	a.forms = append(a.forms, form)
	status, body := a.status, a.body
	a.mu.Unlock()
	if r.URL.Path != "/oauth/v2/token" || r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	if status == 0 {
		status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// oauthConn builds a connector whose HTTP client sends everything to the fake
// but remembers which host the connector meant.
func oauthConn(t *testing.T, a *accountsFake, cfg string) connector.OAuthConnector {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	deps := connectortest.NewDeps(nil, time.Unix(nowUnix, 0))
	deps.HTTP = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		c := r.Clone(r.Context())
		c.Header.Set("X-Original-Host", r.URL.Host)
		c.URL.Scheme, c.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(c)
	})}
	c, err := New(json.RawMessage(cfg), deps)
	if err != nil {
		t.Fatal(err)
	}
	return c.(connector.OAuthConnector)
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// notConnected has the client id and secret but no refresh token yet.
const notConnected = `{"account_id":"acc1","client_id":"cid","client_secret":"csecret","webhook_secret":"` + testSecret + `","widget_api_key":"wkey"}`

func TestZohoIsAnOAuthConnector(t *testing.T) {
	_, info, _ := connector.Lookup("zoho")
	if !info.Has(connector.CapOAuth) {
		t.Fatal("zoho does not declare the oauth capability")
	}
	c := oauthConn(t, &accountsFake{}, notConnected)
	want := "ZohoPay.payments.CREATE,ZohoPay.payments.READ,ZohoPay.payments.UPDATE,ZohoPay.refunds.CREATE,ZohoPay.refunds.READ," +
		"ZohoPay.settings.CREATE,ZohoPay.settings.READ,ZohoPay.settings.UPDATE,ZohoPay.settings.DELETE," +
		"ZohoPay.customers.CREATE,ZohoPay.customers.READ,ZohoPay.payouts.READ"
	if got := strings.Join(c.OAuthDefaultScopes(json.RawMessage(notConnected)), ","); got != want {
		t.Errorf("default scopes\n got %s\nwant %s", got, want)
	}
	sandbox := `{"account_id":"a","client_id":"c","client_secret":"s","webhook_secret":"w","widget_api_key":"k","sandbox":true}`
	if got := strings.Join(c.OAuthDefaultScopes(json.RawMessage(sandbox)), ","); got != strings.ReplaceAll(want, "ZohoPay.", "ZohoPaySandbox.") {
		t.Errorf("sandbox scopes %s", got)
	}
}

func TestAuthorizeURL(t *testing.T) {
	c := oauthConn(t, &accountsFake{}, notConnected)
	redirect := "https://pay.example.com/dashboard/oauth/callback"
	scopes := []string{"ZohoPay.payments.READ", "ZohoPay.refunds.CREATE"}
	raw, err := c.OAuthAuthorizeURL(json.RawMessage(notConnected), redirect, "st4te", scopes)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	if u.Scheme != "https" || u.Host != "accounts.zoho.in" || u.Path != "/oauth/v2/auth" {
		t.Errorf("url = %s", raw)
	}
	for k, v := range map[string]string{"response_type": "code", "access_type": "offline", "prompt": "consent", "client_id": "cid",
		"redirect_uri": redirect, "scope": "ZohoPay.payments.READ,ZohoPay.refunds.CREATE", "state": "st4te"} {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if strings.Contains(raw, "csecret") {
		t.Error("the client secret is in the authorize URL")
	}
	// The data centre decides the host.
	for dc, host := range map[string]string{"eu": "accounts.zoho.eu", "com": "accounts.zoho.com", "com.au": "accounts.zoho.com.au",
		"jp": "accounts.zoho.jp", "sa": "accounts.zoho.sa", "ca": "accounts.zohocloud.ca"} {
		cfg := strings.Replace(notConnected, `"widget_api_key"`, `"data_center":"`+dc+`","widget_api_key"`, 1)
		raw, err := c.OAuthAuthorizeURL(json.RawMessage(cfg), redirect, "s", scopes)
		if u, _ := url.Parse(raw); err != nil || u.Host != host {
			t.Errorf("dc %s: %s %v, want host %s", dc, raw, err, host)
		}
	}
	// Refused.
	for name, c2 := range map[string]string{
		"no client id": strings.Replace(notConnected, `"client_id":"cid"`, `"client_id":""`, 1),
		"bad dc":       strings.Replace(notConnected, `"widget_api_key"`, `"data_center":"evil.com/x#","widget_api_key"`, 1),
	} {
		if _, err := c.OAuthAuthorizeURL(json.RawMessage(c2), redirect, "s", scopes); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := c.OAuthAuthorizeURL(json.RawMessage(notConnected), redirect, "", scopes); err == nil {
		t.Error("no state accepted")
	}
	if _, err := c.OAuthAuthorizeURL(json.RawMessage(notConnected), "", "s", scopes); err == nil {
		t.Error("no redirect URI accepted")
	}
}

// The callback may report the data centre the account lives in. Only Zoho's own
// accounts hosts are followed, whatever the parameters say.
func TestCallbackDataCentreAllowlist(t *testing.T) {
	c := oauthConn(t, &accountsFake{}, notConnected)
	dcOf := func(raw json.RawMessage) string {
		var m struct {
			DC string `json:"data_center"`
		}
		_ = json.Unmarshal(raw, &m)
		return m.DC
	}
	good := map[string]string{ // callback query -> data centre
		"accounts-server=https://accounts.zoho.eu":      "eu",
		"accounts-server=https://accounts.zoho.com":     "com",
		"accounts-server=https://accounts.zoho.com.au":  "com.au",
		"accounts-server=https://accounts.zohocloud.ca": "ca",
		"location=eu": "eu",
		"location=us": "com",
		"location=in": "in",
		"location=au": "com.au",
		"location=jp": "jp",
		"location=ca": "ca",
		"location=cn": "com.cn",
		"location=sa": "sa",
		"":            "",
	}
	for q, want := range good {
		params, _ := url.ParseQuery(q)
		out, err := c.OAuthCallbackConfig(json.RawMessage(notConnected), params)
		if err != nil {
			t.Errorf("%q: %v", q, err)
			continue
		}
		if got := dcOf(out); got != want {
			t.Errorf("%q: data_center = %q, want %q", q, got, want)
		}
	}
	for _, q := range []string{
		"accounts-server=https://evil.example.com",
		"accounts-server=http://accounts.zoho.in",
		"accounts-server=https://accounts.zoho.in.evil.com",
		"accounts-server=https://evil.com/accounts.zoho.in",
		"accounts-server=https://accounts.zoho.in:8443",
		"accounts-server=https://user@accounts.zoho.in",
		"accounts-server=https://accounts.zoho.in/oauth/v2",
		"accounts-server=https://127.0.0.1",
		"accounts-server=https://169.254.169.254",
		"accounts-server=https://payments.zoho.in",
		"location=evil.com",
		"location=in.evil.com",
		"location=",
	} {
		params, _ := url.ParseQuery(q)
		if q == "location=" {
			// An empty value says nothing.
			if _, err := c.OAuthCallbackConfig(json.RawMessage(notConnected), params); err != nil {
				t.Errorf("%q: %v", q, err)
			}
			continue
		}
		if out, err := c.OAuthCallbackConfig(json.RawMessage(notConnected), params); err == nil {
			t.Errorf("%q was accepted: %s", q, out)
		}
	}
}

func TestExchangeTradesTheCodeForARefreshToken(t *testing.T) {
	a := &accountsFake{body: map[string]any{"access_token": "at_secret", "refresh_token": "rt_secret", "scope": "ZohoPay.payments.READ ZohoPay.refunds.CREATE",
		"api_domain": "https://www.zohoapis.in", "token_type": "Bearer", "expires_in": 3600}}
	c := oauthConn(t, a, notConnected)
	redirect := "https://pay.example.com/dashboard/oauth/callback"
	res, err := c.OAuthExchange(context.Background(), json.RawMessage(notConnected), redirect, "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if res.RefreshToken != "rt_secret" || res.ExpiresIn != 3600 || strings.Join(res.Scopes, ",") != "ZohoPay.payments.READ,ZohoPay.refunds.CREATE" {
		t.Fatalf("result %+v", res)
	}
	if res.ConfigPatch["refresh_token"] != "rt_secret" || res.ConfigPatch["data_center"] != "in" ||
		res.ConfigPatch["oauth_scopes"] != "ZohoPay.payments.READ,ZohoPay.refunds.CREATE" {
		t.Errorf("config patch %v", res.ConfigPatch)
	}
	if _, leaked := res.ConfigPatch["access_token"]; leaked {
		t.Error("the access token was put in the config")
	}
	f := a.forms[0]
	for k, v := range map[string]string{"grant_type": "authorization_code", "client_id": "cid", "client_secret": "csecret", "code": "the-code", "redirect_uri": redirect} {
		if f.Get(k) != v {
			t.Errorf("token request %s = %q, want %q", k, f.Get(k), v)
		}
	}
	if a.hosts[0] != "accounts.zoho.in" {
		t.Errorf("token request went to %s", a.hosts[0])
	}
	// A Self Client's code has no redirect.
	if _, err := c.OAuthExchange(context.Background(), json.RawMessage(notConnected), "", "self-code"); err != nil {
		t.Fatal(err)
	}
	if a.forms[1].Has("redirect_uri") {
		t.Error("a redirect_uri was sent for a self-client code")
	}
	// Another data centre goes to that data centre's accounts host.
	eu := strings.Replace(notConnected, `"widget_api_key"`, `"data_center":"eu","widget_api_key"`, 1)
	if _, err := c.OAuthExchange(context.Background(), json.RawMessage(eu), redirect, "c"); err != nil {
		t.Fatal(err)
	}
	if a.hosts[2] != "accounts.zoho.eu" {
		t.Errorf("token request went to %s", a.hosts[2])
	}
}

func TestExchangeFailuresSayWhyWithoutLeakingSecrets(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"invalid code":     {"error": "invalid_code"},
		"no refresh token": {"access_token": "at_only", "expires_in": 3600},
		"invalid client":   {"error": "invalid_client"},
	} {
		a := &accountsFake{body: body}
		c := oauthConn(t, a, notConnected)
		_, err := c.OAuthExchange(context.Background(), json.RawMessage(notConnected), "https://x.example.com/cb", "c0de")
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		for _, secret := range []string{"csecret", "at_only", "c0de"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error leaks %q: %v", name, secret, err)
			}
		}
	}
	a := &accountsFake{status: 503, body: map[string]any{}}
	c := oauthConn(t, a, notConnected)
	if _, err := c.OAuthExchange(context.Background(), json.RawMessage(notConnected), "", "c"); !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("a 503: %v", err)
	}
	if _, err := c.OAuthExchange(context.Background(), json.RawMessage(notConnected), "", ""); err == nil {
		t.Error("an empty code was exchanged")
	}
}

// An account that has not been connected yet can be created, and says so
// clearly when used, without calling Zoho.
func TestAnAccountWithoutARefreshTokenIsNotConnected(t *testing.T) {
	a := &accountsFake{}
	f := newFake("succeeded")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(json.RawMessage(notConnected), connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatalf("an account without a refresh token must be constructible: %v", err)
	}
	_ = a
	_, err = c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ps_1"})
	if !errors.Is(err, connector.ErrInvalidCredentials) || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("err = %v, want invalid credentials saying the account is not connected", err)
	}
	if f.tokenCalls != 0 || len(f.calls) != 0 {
		t.Errorf("Zoho was called (%d token calls, %v) for an account with no refresh token", f.tokenCalls, f.calls)
	}
	// The other credentials are still required.
	if _, err := New(json.RawMessage(strings.Replace(notConnected, `"client_secret":"csecret"`, `"client_secret":""`, 1)), connectortest.NewDeps(srv, time.Unix(nowUnix, 0))); err == nil {
		t.Error("an account without a client secret was accepted")
	}
}

func TestADataCentreOutsideZohosListIsRefused(t *testing.T) {
	bad := strings.Replace(notConnected, `"widget_api_key"`, `"data_center":"evil.example.com/","widget_api_key"`, 1)
	if _, err := New(json.RawMessage(bad), connectortest.NewDeps(nil, time.Unix(nowUnix, 0))); err == nil {
		t.Error("an arbitrary data_center was accepted: it builds the host the connector calls")
	}
}
