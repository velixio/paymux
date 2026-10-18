package dashboard

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/paymux/paymux/connectors/zoho"
	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

// oauthRecorder and oauthKnown, when a test sets them, are the call recorder
// and the known-secrets hook of the OAuth tests' connectors (the error log).
var (
	oauthRecorder func(store.ProviderAccount) connector.CallRecorder
	oauthKnown    func(owner string, values []string)
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func orgScope() core.OrgScope { return core.OrgScope{OrgID: store.DefaultOrgID} }

// "Connect with Zoho": the operator registers the redirect URI in a
// server-based client, pastes the client id and secret, approves at Zoho, and
// the refresh token is stored without anyone copying a grant code.

const (
	secretClient  = "zoho-client-secret-XYZ"
	secretRefresh = "rt_SECRET_refresh_token"
	secretAccess  = "at_SECRET_access_token"
	callbackURI   = "http://localhost:8080/dashboard/oauth/callback"
)

// zohoAccounts is Zoho's accounts server, as the connector sees it.
type zohoAccounts struct {
	mu    sync.Mutex
	hosts []string
	forms []url.Values
	body  map[string]any
}

func (z *zohoAccounts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(b))
	z.mu.Lock()
	z.hosts = append(z.hosts, r.Header.Get("X-Original-Host"))
	z.forms = append(z.forms, form)
	body := z.body
	z.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (z *zohoAccounts) calls() int { z.mu.Lock(); defer z.mu.Unlock(); return len(z.forms) }

type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *syncLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type oauthEnv struct {
	*env
	zoho *zohoAccounts
	logs *syncLog
	acct string
	pa   store.ProviderAccount
	// admin is the logged-in administrator.
	admin *http.Client
}

const zohoNotConnected = `{"account_id":"acc1","client_id":"old-client-id","client_secret":"` + secretClient + `","webhook_secret":"whsec","widget_api_key":"wkey"}`

func newOAuthEnv(t *testing.T, db *store.DB) *oauthEnv {
	t.Helper()
	z := &zohoAccounts{body: map[string]any{"access_token": secretAccess, "refresh_token": secretRefresh, "expires_in": 3600,
		"scope": "ZohoPay.payments.READ ZohoPay.refunds.CREATE", "token_type": "Bearer"}}
	srv := httptest.NewServer(z)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	logs := &syncLog{}
	e := newEnv(t, db, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		o.Connectors = connectors.New(connectors.Options{DB: o.DB, Box: o.Box, BaseURL: o.BaseURL, Now: o.Now, Logger: o.Logger,
			KnownSecrets: oauthKnown,
			HTTPClient: func(pa store.ProviderAccount, _ connector.Info, _ []string) *http.Client {
				rt := rtFunc(func(r *http.Request) (*http.Response, error) {
					c := r.Clone(r.Context())
					c.Header.Set("X-Original-Host", r.URL.Host)
					c.URL.Scheme, c.URL.Host = target.Scheme, target.Host
					return http.DefaultTransport.RoundTrip(c)
				})
				if oauthRecorder != nil {
					return connector.NewHTTPClient(connector.HTTPOptions{Base: rt, Recorder: oauthRecorder(pa)})
				}
				return &http.Client{Transport: rt}
			}})
	})
	pa, err := e.d.o.Connectors.CreateAccount(ctx, audit.System, orgScope(), connectors.CreateAccountInput{Connector: "zoho", Name: "Zoho", Mode: "test",
		Config: json.RawMessage(zohoNotConnected)})
	if err != nil {
		t.Fatal(err)
	}
	return &oauthEnv{env: e, zoho: z, logs: logs, acct: pa.ID, pa: pa}
}

func (e *oauthEnv) page() string { return "/dashboard/providers/" + e.acct }

// connect posts the Connect form and returns the response.
func (e *oauthEnv) connect(c *http.Client, extra url.Values) *http.Response {
	e.t.Helper()
	tok := csrfOf(e.t, e.pageOK(c, "/dashboard/providers"))
	f := url.Values{"csrf": {tok}, "oauth_client_id": {"new-client-id"}, "oauth_client_secret": {""}}
	for k, v := range extra {
		f[k] = v
	}
	r, body := e.post(c, e.page()+"/oauth/connect", f)
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

// authStateOf reads the authorization URL off the "continue" page. Connect
// must not answer with a cross-origin redirect: the dashboard's CSP
// (form-action 'self') makes browsers block a form submission that redirects
// to another origin, so the page links (and meta-refreshes) to it instead.
func authStateOf(t *testing.T, r *http.Response) string {
	t.Helper()
	return authURLOf(t, r).Query().Get("state")
}

// authURLOf is the authorization URL the continue page links to.
func authURLOf(t *testing.T, r *http.Response) *url.URL {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	body := string(b)
	if r.StatusCode != 200 || r.Header.Get("Location") != "" {
		t.Fatalf("connect answered %d %q; want 200 with a continue page (no cross-origin redirect)", r.StatusCode, r.Header.Get("Location"))
	}
	m := regexp.MustCompile(`<a [^>]*id="oauth-continue"[^>]*href="([^"]+)"`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("no continue link / refresh on the page:\n%s", body)
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("continue link %q has no state", m[1])
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Error("the continue page must not be cached")
	}
	if !strings.Contains(body, `content="0;url=`+m[1]+`"`) {
		t.Errorf("the refresh does not lead to the authorization URL:\n%s", body)
	}
	return u
}

func eachOAuth(t *testing.T, fn func(t *testing.T, e *oauthEnv, admin *http.Client)) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		e := newOAuthEnv(t, db)
		admin := e.roleClient("admin")
		e.admin = admin
		fn(t, e, admin)
	})
}

func TestConnectPanelShowsTheRedirectURIAndTheSteps(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		body := e.pageOK(admin, e.page())
		has(t, body, callbackURI, `data-copy="`+callbackURI+`"`, "Server-based", "api-console.zoho.in", "Not connected",
			`name="oauth_client_id"`, `value="old-client-id"`, `name="oauth_client_secret"`, `type="password"`, `name="oauth_scopes"`,
			"ZohoPay.payments.CREATE", "ZohoPay.payouts.READ", `action="`+e.page()+`/oauth/connect"`, `action="`+e.page()+`/oauth/code"`, `name="grant_code"`)
		lacks(t, body, secretClient) // the secret is masked: only "set" is said
		has(t, body, "leave blank to keep")
		// Only for an admin.
		for _, role := range []string{"viewer", "operator"} {
			b := e.pageOK(e.roleClient(role), e.page())
			lacks(t, b, "oauth/connect", callbackURI, secretClient)
		}
		// Only for a connector that can.
		st := fakeconn.Register("plainfake")
		pa, err := e.d.o.Connectors.CreateAccount(ctx, audit.System, orgScope(), connectors.CreateAccountInput{Connector: st.Name, Name: "Plain", Mode: "test",
			Config: json.RawMessage(fakeconn.ValidConfig)})
		if err != nil {
			t.Fatal(err)
		}
		lacks(t, e.pageOK(admin, "/dashboard/providers/"+pa.ID), "oauth/connect", "Server-based")
	})
}

func TestConnectSavesTheClientAndSendsTheOperatorToZoho(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		r := e.connect(admin, url.Values{"oauth_scopes": {"ZohoPay.payments.READ\nZohoPay.refunds.CREATE"}})
		u := authURLOf(t, r)
		state := u.Query().Get("state")
		q := u.Query()
		if u.Host != "accounts.zoho.in" || u.Path != "/oauth/v2/auth" {
			t.Errorf("authorization URL %s", u)
		}
		for k, v := range map[string]string{"response_type": "code", "access_type": "offline", "prompt": "consent", "client_id": "new-client-id",
			"redirect_uri": callbackURI, "scope": "ZohoPay.payments.READ,ZohoPay.refunds.CREATE"} {
			if q.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
		if strings.Contains(u.String(), secretClient) {
			t.Error("the client secret is in the authorization URL")
		}
		// The client id was saved, the blank secret kept.
		cfg := e.openConfig(e.acct)
		if cfg["client_id"] != "new-client-id" || cfg["client_secret"] != secretClient {
			t.Errorf("config client_id=%v client_secret kept=%v", cfg["client_id"], cfg["client_secret"] == secretClient)
		}
		// The state is stored server-side, hashed, for ten minutes.
		var n int
		var raw int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			_ = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM oauth_states").Scan(&n)
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM oauth_states WHERE state_hash = ?", state).Scan(&raw)
		})
		if n != 1 || raw != 0 {
			t.Errorf("states stored = %d, raw state stored = %d", n, raw)
		}
		// A new secret replaces the old one.
		e.connect(admin, url.Values{"oauth_client_secret": {"another-secret"}})
		if e.openConfig(e.acct)["client_secret"] != "another-secret" {
			t.Error("a new secret was not saved")
		}
		// No client at all.
		cfg2 := e.openConfig(e.acct)
		_ = cfg2
		if r := e.connect(admin, url.Values{"oauth_client_id": {""}, "oauth_scopes": {"bad scope!"}}); r.StatusCode != 400 {
			t.Errorf("a malformed scope: %d", r.StatusCode)
		}
		// Not for the unprivileged, nor without the CSRF token.
		for _, role := range []string{"viewer", "operator"} {
			if r := e.connect(e.roleClient(role), nil); r.StatusCode != 403 {
				t.Errorf("%s connect: %d", role, r.StatusCode)
			}
		}
		if r, _ := e.post(admin, e.page()+"/oauth/connect", url.Values{"oauth_client_id": {"x"}}); r.StatusCode != 403 {
			t.Errorf("without csrf: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/providers/pa_nosuch/oauth/connect", url.Values{"csrf": {csrfOf(t, e.pageOK(admin, e.page()))}}); r.StatusCode != 404 {
			t.Errorf("unknown account: %d", r.StatusCode)
		}
	})
}

func (e *oauthEnv) callback(c *http.Client, query string) (*http.Response, string) {
	e.t.Helper()
	return e.get(c, "/dashboard/oauth/callback?"+query)
}

func TestCallbackStoresTheRefreshTokenAndNeverShowsIt(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		state := authStateOf(t, e.connect(admin, url.Values{"oauth_client_secret": {secretClient}}))
		r, body := e.callback(admin, "code=the-grant-code&state="+url.QueryEscape(state))
		if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), e.page()) {
			t.Fatalf("callback: %d %q\n%s", r.StatusCode, r.Header.Get("Location"), body)
		}
		// The exchange: server side, with the redirect URI, at the account's data centre.
		f := e.zoho.forms[0]
		for k, v := range map[string]string{"grant_type": "authorization_code", "code": "the-grant-code", "redirect_uri": callbackURI,
			"client_id": "new-client-id", "client_secret": secretClient} {
			if f.Get(k) != v {
				t.Errorf("token request %s = %q, want %q", k, f.Get(k), v)
			}
		}
		if e.zoho.hosts[0] != "accounts.zoho.in" {
			t.Errorf("exchange host %s", e.zoho.hosts[0])
		}
		cfg := e.openConfig(e.acct)
		if cfg["refresh_token"] != secretRefresh || cfg["oauth_scopes"] != "ZohoPay.payments.READ,ZohoPay.refunds.CREATE" || cfg["client_id"] != "new-client-id" {
			t.Errorf("stored config: %v", cfg)
		}
		if _, ok := cfg["access_token"]; ok {
			t.Error("the access token was stored")
		}
		// The account page says connected, lists the scopes, and holds no secret.
		page := e.pageOK(admin, r.Header.Get("Location"))
		has(t, page, "Connected", "ZohoPay.payments.READ", "ZohoPay.refunds.CREATE", "Reconnect")
		lacks(t, page, secretRefresh, secretAccess, secretClient, "the-grant-code")
		// The audit trail records the connection without a secret.
		var entries []string
		_ = e.db.Read(ctx, func(q store.Querier) error {
			rows, err := q.QueryContext(ctx, "SELECT action || ' ' || COALESCE(before,'') || ' ' || COALESCE(after,'') FROM audit_log WHERE action LIKE 'provider_account.%'")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				entries = append(entries, s)
			}
			return nil
		})
		joined := strings.Join(entries, "\n")
		if !strings.Contains(joined, "provider_account.oauth_connect") || !strings.Contains(joined, "ZohoPay.payments.READ") {
			t.Errorf("no audit entry for the connection:\n%s", joined)
		}
		for _, secret := range []string{secretRefresh, secretAccess, secretClient, "the-grant-code", state} {
			if strings.Contains(joined, secret) {
				t.Errorf("the audit log holds %q", secret)
			}
			if strings.Contains(e.logs.String(), secret) {
				t.Errorf("the logs hold %q", secret)
			}
		}
		// Saving the settings form afterwards keeps the token.
		tok := csrfOf(t, e.pageOK(admin, e.page()))
		if r, _ := e.post(admin, e.page(), url.Values{"csrf": {tok}, "name": {"Zoho renamed"}, "status": {"active"}, "cfg_account_id": {"acc1"},
			"cfg_client_id": {"new-client-id"}, "cfg_webhook_secret": {""}, "cfg_widget_api_key": {"wkey"}, "currency": {"INR"}, "purpose": {"one_time"}}); r.StatusCode != 303 {
			t.Fatalf("settings save: %d", r.StatusCode)
		}
		if cfg := e.openConfig(e.acct); cfg["refresh_token"] != secretRefresh || cfg["oauth_scopes"] == nil {
			t.Errorf("saving the settings dropped the connection: %v", cfg)
		}
	})
}

func TestCallbackRefusesAStateThatIsWrongExpiredOrReused(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		stored := func() bool { _, ok := e.openConfig(e.acct)["refresh_token"]; return ok }
		// Tampered, empty, or never issued.
		good := authStateOf(t, e.connect(admin, nil))
		for name, q := range map[string]string{
			"tampered": "code=c&state=" + url.QueryEscape(good+"x"),
			"empty":    "code=c&state=",
			"missing":  "code=c",
			"unknown":  "code=c&state=never-issued",
		} {
			if r, _ := e.callback(admin, q); r.StatusCode != 400 {
				t.Errorf("%s: %d", name, r.StatusCode)
			}
		}
		if e.zoho.calls() != 0 || stored() {
			t.Fatal("a bad state led to an exchange or stored a token")
		}
		// The real one works once; a replay is refused and exchanges nothing.
		if r, _ := e.callback(admin, "code=c&state="+url.QueryEscape(good)); r.StatusCode != 303 {
			t.Fatalf("first use: %d", r.StatusCode)
		}
		before := e.zoho.calls()
		if r, _ := e.callback(admin, "code=c2&state="+url.QueryEscape(good)); r.StatusCode != 400 {
			t.Errorf("replay: %d", r.StatusCode)
		}
		if e.zoho.calls() != before {
			t.Error("a replayed state led to an exchange")
		}
		// Expired after ten minutes.
		old := authStateOf(t, e.connect(admin, nil))
		e.clk.Add(11 * time.Minute)
		if r, _ := e.callback(admin, "code=c&state="+url.QueryEscape(old)); r.StatusCode != 400 {
			t.Errorf("expired: %d", r.StatusCode)
		}
		if e.zoho.calls() != before {
			t.Error("an expired state led to an exchange")
		}
	})
}

// A state belongs to the user who began the authorization.
func TestCallbackStateIsBoundToTheUserWhoBeganIt(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		state := authStateOf(t, e.connect(admin, nil))
		_, secret := e.seedUser("second@example.com", "admin", true)
		other := e.client()
		e.login(other, "second@example.com", secret)
		if r, _ := e.callback(other, "code=c&state="+url.QueryEscape(state)); r.StatusCode != 400 {
			t.Errorf("another admin used the state: %d", r.StatusCode)
		}
		if e.zoho.calls() != 0 {
			t.Error("an exchange was made for another user's state")
		}
		// The state was spent by the attempt; the owner cannot use it either.
		if r, _ := e.callback(admin, "code=c&state="+url.QueryEscape(state)); r.StatusCode != 400 {
			t.Errorf("spent state: %d", r.StatusCode)
		}
	})
}

func TestCallbackNeedsASessionAndPermission(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		state := authStateOf(t, e.connect(admin, nil))
		q := "code=c&state=" + url.QueryEscape(state)
		if r, _ := e.callback(e.client(), q); r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/dashboard/login") {
			t.Errorf("no session: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
		for _, role := range []string{"viewer", "operator"} {
			if r, _ := e.callback(e.roleClient(role), q); r.StatusCode != 403 {
				t.Errorf("%s: %d", role, r.StatusCode)
			}
		}
		if e.zoho.calls() != 0 {
			t.Error("an unauthorised callback led to an exchange")
		}
		// The state was not spent by them: the admin can still finish.
		if r, _ := e.callback(admin, q); r.StatusCode != 303 {
			t.Errorf("the admin: %d", r.StatusCode)
		}
	})
}

func TestCallbackDenialAndFailureStoreNothing(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		state := authStateOf(t, e.connect(admin, nil))
		r, _ := e.callback(admin, "error=access_denied&state="+url.QueryEscape(state))
		if r.StatusCode != 303 || !strings.Contains(r.Header.Get("Location"), e.page()) {
			t.Fatalf("denied: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
		page := e.pageOK(admin, r.Header.Get("Location"))
		has(t, page, "was not approved")
		if e.zoho.calls() != 0 {
			t.Error("a denied authorization led to an exchange")
		}
		if _, ok := e.openConfig(e.acct)["refresh_token"]; ok {
			t.Error("a token was stored")
		}
		// Zoho refuses the code: the page says so, clearly, and nothing is stored.
		e.zoho.mu.Lock()
		e.zoho.body = map[string]any{"error": "invalid_code"}
		e.zoho.mu.Unlock()
		state = authStateOf(t, e.connect(admin, nil))
		r, body := e.callback(admin, "code=stale-code&state="+url.QueryEscape(state))
		if r.StatusCode == 303 || !strings.Contains(body, "invalid_code") {
			t.Errorf("a refused code: %d\n%s", r.StatusCode, body)
		}
		lacks(t, body, "stale-code", secretClient)
		if _, ok := e.openConfig(e.acct)["refresh_token"]; ok {
			t.Error("a token was stored after a failed exchange")
		}
	})
}

// Zoho names the data centre the account lives in; only Zoho's own hosts are followed.
func TestCallbackDataCentreComesFromZohosListOnly(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		state := authStateOf(t, e.connect(admin, nil))
		r, _ := e.callback(admin, "code=c&state="+url.QueryEscape(state)+"&location=eu&accounts-server="+url.QueryEscape("https://accounts.zoho.eu"))
		if r.StatusCode != 303 {
			t.Fatalf("eu: %d", r.StatusCode)
		}
		if e.zoho.hosts[0] != "accounts.zoho.eu" {
			t.Errorf("exchange host %s", e.zoho.hosts[0])
		}
		if e.openConfig(e.acct)["data_center"] != "eu" {
			t.Errorf("data centre not recorded: %v", e.openConfig(e.acct)["data_center"])
		}
		before := e.zoho.calls()
		for _, evil := range []string{"https://evil.example.com", "http://accounts.zoho.in", "https://accounts.zoho.in.evil.com", "https://127.0.0.1", "https://169.254.169.254"} {
			state := authStateOf(t, e.connect(admin, nil))
			if r, _ := e.callback(admin, "code=c&state="+url.QueryEscape(state)+"&accounts-server="+url.QueryEscape(evil)); r.StatusCode != 400 {
				t.Errorf("%s: %d", evil, r.StatusCode)
			}
		}
		if e.zoho.calls() != before {
			t.Error("a host outside Zoho's list was called")
		}
	})
}

func TestPastingAGrantCodeConnectsWithoutARedirect(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		tok := csrfOf(t, e.pageOK(admin, e.page()))
		r, body := e.post(admin, e.page()+"/oauth/code", url.Values{"csrf": {tok}, "grant_code": {"1000.selfclientcode.abc"}, "oauth_client_id": {"self-id"}, "oauth_client_secret": {"self-secret"}})
		if r.StatusCode != 303 {
			t.Fatalf("paste: %d\n%s", r.StatusCode, body)
		}
		f := e.zoho.forms[0]
		if f.Get("code") != "1000.selfclientcode.abc" || f.Has("redirect_uri") || f.Get("client_id") != "self-id" || f.Get("client_secret") != "self-secret" {
			t.Errorf("token request %v", f)
		}
		if cfg := e.openConfig(e.acct); cfg["refresh_token"] != secretRefresh || cfg["client_id"] != "self-id" {
			t.Errorf("config %v", cfg)
		}
		for _, s := range []string{secretRefresh, "1000.selfclientcode.abc", "self-secret"} {
			if strings.Contains(e.logs.String(), s) {
				t.Errorf("the logs hold %q", s)
			}
			if strings.Contains(e.pageOK(admin, e.page()), s) {
				t.Errorf("the page holds %q", s)
			}
		}
		// A blank code, no permission, no CSRF.
		if r, _ := e.post(admin, e.page()+"/oauth/code", url.Values{"csrf": {tok}, "grant_code": {" "}}); r.StatusCode != 400 {
			t.Errorf("blank code: %d", r.StatusCode)
		}
		if r, _ := e.post(e.roleClient("operator"), e.page()+"/oauth/code", url.Values{"csrf": {csrfOf(t, e.pageOK(e.roleClient("viewer"), "/dashboard/providers"))}, "grant_code": {"x"}}); r.StatusCode != 403 {
			t.Errorf("operator: %d", r.StatusCode)
		}
	})
}

// An account can be created without a refresh token and connected afterwards;
// until then it says so instead of failing obscurely.
func TestAnAccountWithoutATokenCanBeCreatedAndSaysItIsNotConnected(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		body := e.pageOK(admin, "/dashboard/providers/new?connector=zoho")
		tok := csrfOf(t, body)
		has(t, body, "refresh token", `name="cfg_client_id"`)
		r, out := e.post(admin, "/dashboard/providers", url.Values{"csrf": {tok}, "connector": {"zoho"}, "name": {"Zoho live"}, "mode": {"live"},
			"cfg_account_id": {"acc9"}, "cfg_client_id": {"cid9"}, "cfg_client_secret": {"csec9"}, "cfg_webhook_secret": {"wh9"}, "cfg_widget_api_key": {"wk9"},
			"cfg_refresh_token": {""}, "currency": {"INR"}, "purpose": {"one_time"}})
		if r.StatusCode != 303 {
			t.Fatalf("create without a token: %d\n%s", r.StatusCode, out)
		}
		page := r.Header.Get("Location")
		has(t, e.pageOK(admin, page), "Not connected", "oauth/connect")
		r, out = e.post(admin, page+"/test", url.Values{"csrf": {csrfOf(t, e.pageOK(admin, page))}})
		if r.StatusCode != 200 || !strings.Contains(strings.ToLower(out), "not connected") {
			t.Errorf("testing an unconnected account: %d\n%s", r.StatusCode, out)
		}
	})
}

// The panel follows the dashboard's phone-friendly style.
func TestConnectPanelIsPhoneFriendly(t *testing.T) {
	eachOAuth(t, func(t *testing.T, e *oauthEnv, admin *http.Client) {
		body := e.pageOK(admin, e.page())
		i := strings.Index(body, `id="connect"`)
		if i < 0 {
			t.Fatal("no connect panel")
		}
		panel := body[i:]
		has(t, panel, `class="stack"`, `class="field"`, `class="wrap"`, `class="linkbtn"`)
		lacks(t, panel, "<table", "style=\"width", "width=\"")
	})
}
