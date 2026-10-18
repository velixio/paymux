package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/authz"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// "Connect": for a connector that is an OAuth client of its provider
// (connector.OAuthConnector), the provider account page offers to authorize
// the account at the provider and keep the refresh token that comes back, so
// nobody pastes grant codes by hand.
//
// The flow: the operator registers this hub's callback URL in the provider's
// OAuth client, enters the client id and secret, and presses Connect. Paymux
// saves them, makes a random single-use state bound to the user and the
// account, and sends the operator to the provider. The provider returns to
// /dashboard/oauth/callback with a code and the state; Paymux checks and spends
// the state, exchanges the code on the server, and seals the refresh token into
// the account's config. Tokens and the client secret are never rendered, logged
// or written to the audit log.

const (
	oauthStateTTL = 10 * time.Minute
	maxScopes     = 40
)

var scopeRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)

var oauthNotices = map[string]string{
	"oauth_ok": "Connected. Press Test connection to check that the provider accepts the account.",
}

func (d *Dashboard) routesOAuth() {
	m := d.mux
	m.HandleFunc("POST "+prefix+"/providers/{id}/oauth/connect", d.authed(authz.ProviderAccountManage, d.oauthConnect))
	m.HandleFunc("POST "+prefix+"/providers/{id}/oauth/code", d.authed(authz.ProviderAccountManage, d.oauthPasteCode))
	m.HandleFunc("GET "+prefix+"/oauth/callback", d.authed(authz.ProviderAccountManage, d.oauthCallback))
}

// oauthPanel is what the account page shows for a connector that can connect.
type oauthPanel struct {
	Display, RedirectURI, ConsoleHost string
	IsZoho, Connected, HasSecret      bool
	ClientID                          string
	Granted                           []string
	ScopesText                        string
}

func (d *Dashboard) redirectURI() string {
	return strings.TrimRight(d.o.BaseURL.String(), "/") + prefix + "/oauth/callback"
}

// configRoles finds, from the connector's schema, which config properties
// hold the OAuth client id, secret, refresh token and granted scopes.
func configRoles(info connector.Info) map[string]string {
	roles := map[string]string{}
	for _, f := range schemaFields(info.ConfigSchema) {
		if f.OAuthRole != "" {
			roles[f.OAuthRole] = f.Name
		}
	}
	return roles
}

func str(m map[string]any, key string) string {
	if key == "" {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func (d *Dashboard) buildOAuthPanel(ctx context.Context, pa store.ProviderAccount, info connector.Info, stored map[string]any, post url.Values) *oauthPanel {
	roles := configRoles(info)
	p := &oauthPanel{Display: firstNonBlank(info.DisplayName, info.Name), RedirectURI: d.redirectURI(), IsZoho: info.Name == "zoho",
		ClientID: str(stored, roles["client_id"]), HasSecret: str(stored, roles["client_secret"]) != "",
		Connected: str(stored, roles["refresh_token"]) != ""}
	if s := str(stored, roles["scopes"]); s != "" {
		p.Granted = strings.Split(s, ",")
	}
	if post != nil && post.Has("oauth_client_id") {
		p.ClientID = post.Get("oauth_client_id")
	}
	var scopes []string
	if raw, err := json.Marshal(stored); err == nil {
		if live, err := d.o.Connectors.Get(ctx, pa.ID); err == nil {
			if oc, ok := live.Connector.(connector.OAuthConnector); ok {
				scopes = oc.OAuthDefaultScopes(raw)
				if u, err := oc.OAuthAuthorizeURL(raw, p.RedirectURI, "x", scopes); err == nil {
					if pu, err := url.Parse(u); err == nil {
						p.ConsoleHost = "api-console." + strings.TrimPrefix(pu.Host, "accounts.")
					}
				}
			}
		}
	}
	if post != nil && post.Has("oauth_scopes") {
		p.ScopesText = post.Get("oauth_scopes")
	} else {
		p.ScopesText = strings.Join(scopes, "\n")
	}
	return p
}

// parseScopes reads scopes separated by newlines, spaces or commas.
func parseScopes(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ' ' || r == ',' || r == '\t' }) {
		if !scopeRE.MatchString(f) {
			return nil, fieldErr("A scope may only have letters, digits and . _ : - (check \"" + truncate(f, 30) + "\").")
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) > maxScopes {
		return nil, fieldErr("Too many scopes.")
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// oauthTarget loads the account and checks its connector can connect.
func (d *Dashboard) oauthTarget(w http.ResponseWriter, r *http.Request) (pa store.ProviderAccount, scope core.OrgScope, info connector.Info, ok bool) {
	pa, scope, ok = d.loadProvider(w, r)
	if !ok {
		return
	}
	_, info, known := connector.Lookup(pa.Connector)
	if !known || !info.Has(connector.CapOAuth) {
		http.NotFound(w, r)
		return pa, scope, info, false
	}
	return pa, scope, info, true
}

// withClient saves the client id and secret the operator entered (a blank
// secret keeps the stored one) and returns the config now in force.
func (d *Dashboard) withClient(r *http.Request, s *session, pa store.ProviderAccount, scope core.OrgScope, info connector.Info) (map[string]any, error) {
	stored := d.storedConfig(pa)
	if stored == nil {
		stored = map[string]any{}
	}
	roles := configRoles(info)
	changed := false
	if id := strings.TrimSpace(r.PostForm.Get("oauth_client_id")); id != "" && id != str(stored, roles["client_id"]) {
		stored[roles["client_id"]] = id
		changed = true
	}
	if secret := strings.TrimSpace(r.PostForm.Get("oauth_client_secret")); secret != "" && secret != str(stored, roles["client_secret"]) {
		stored[roles["client_secret"]] = secret
		changed = true
	}
	if str(stored, roles["client_id"]) == "" || str(stored, roles["client_secret"]) == "" {
		return nil, fieldErr("Enter the OAuth client id and secret.")
	}
	if changed {
		raw, _ := json.Marshal(stored)
		if err := d.o.Connectors.UpdateAccount(r.Context(), s.auditActor(), scope, pa.ID, connectors.UpdateAccountInput{Config: raw}); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

func (d *Dashboard) oauthFail(w http.ResponseWriter, r *http.Request, s *session, err error) {
	var fe fieldErr
	switch {
	case errors.As(err, &fe), errors.Is(err, core.ErrInvalid):
		d.renderProvider(w, r, s, http.StatusBadRequest, err.Error(), "", false, nil)
	case errors.Is(err, core.ErrNotFound):
		http.NotFound(w, r)
	default:
		d.fail(w, err)
	}
}

func newState() (state, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(b[:])
	return state, hashState(state), nil
}

func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// oauthConnect saves the client and sends the operator to the provider.
func (d *Dashboard) oauthConnect(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, info, ok := d.oauthTarget(w, r)
	if !ok {
		return
	}
	scopes, err := parseScopes(r.PostForm.Get("oauth_scopes"))
	if err != nil {
		d.oauthFail(w, r, s, err)
		return
	}
	cfg, err := d.withClient(r, s, pa, scope, info)
	if err != nil {
		d.oauthFail(w, r, s, err)
		return
	}
	raw, _ := json.Marshal(cfg)
	live, err := d.o.Connectors.Get(r.Context(), pa.ID)
	if err != nil {
		d.fail(w, err)
		return
	}
	oc, ok := live.Connector.(connector.OAuthConnector)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if len(scopes) == 0 {
		scopes = oc.OAuthDefaultScopes(raw)
	}
	state, hash, err := newState()
	if err != nil {
		d.fail(w, err)
		return
	}
	target, err := oc.OAuthAuthorizeURL(raw, d.redirectURI(), state, scopes)
	if err != nil {
		d.oauthFail(w, r, s, fieldErr(err.Error()))
		return
	}
	now := d.o.Now()
	err = d.o.DB.Write(r.Context(), func(tx store.Tx) error {
		return store.InsertOAuthState(r.Context(), tx, store.OAuthState{Hash: hash, OrgID: scope.OrgID, UserID: s.User.ID, AccountID: pa.ID,
			ExpiresAt: now.Add(oauthStateTTL)}, now)
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// oauthCallback is where the provider sends the operator back.
func (d *Dashboard) oauthCallback(w http.ResponseWriter, r *http.Request, s *session) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()
	state := q.Get("state")
	scope, err := d.orgScope(r)
	if err != nil {
		d.fail(w, err)
		return
	}
	bad := func(msg string) { http.Error(w, msg, http.StatusBadRequest) }
	if state == "" {
		bad("This authorization is invalid or has expired. Start again from the provider account page.")
		return
	}
	var st store.OAuthState
	err = d.o.DB.Write(r.Context(), func(tx store.Tx) (err error) {
		st, err = store.ConsumeOAuthState(r.Context(), tx, hashState(state), d.o.Now())
		return
	})
	if errors.Is(err, store.ErrNotFound) || err == nil && (st.UserID != s.User.ID || st.OrgID != scope.OrgID) {
		bad("This authorization is invalid or has expired. Start again from the provider account page.")
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	r.SetPathValue("id", st.AccountID)
	back := prefix + "/providers/" + st.AccountID
	// Pages rendered from here must not carry the code or the state: the
	// request URI ends up in the page's forms (the mode switch's "next").
	r.URL.Path, r.URL.RawQuery = back, ""
	if e := q.Get("error"); e != "" {
		// The provider's own error text is not shown: it is not ours to render.
		if e == "access_denied" {
			http.Redirect(w, r, back+"?n=oauth_denied", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, back+"?n=oauth_failed", http.StatusSeeOther)
		}
		return
	}
	code := q.Get("code")
	if code == "" {
		bad("The provider returned no authorization code.")
		return
	}
	pa, _, info, ok := d.oauthTarget(w, r)
	if !ok {
		return
	}
	stored := d.storedConfig(pa)
	raw, _ := json.Marshal(stored)
	live, err := d.o.Connectors.Get(r.Context(), pa.ID)
	if err != nil {
		d.fail(w, err)
		return
	}
	oc, ok := live.Connector.(connector.OAuthConnector)
	if !ok {
		http.NotFound(w, r)
		return
	}
	adjusted, err := oc.OAuthCallbackConfig(raw, q)
	if err != nil {
		bad("The provider's callback was refused: " + err.Error())
		return
	}
	d.finishConnect(w, r, s, oc, pa, scope, info, adjusted, d.redirectURI(), code, "redirect")
}

// oauthPasteCode connects with a grant code the operator pastes (a self
// client's code, which has no redirect).
func (d *Dashboard) oauthPasteCode(w http.ResponseWriter, r *http.Request, s *session) {
	pa, scope, info, ok := d.oauthTarget(w, r)
	if !ok {
		return
	}
	code := strings.TrimSpace(r.PostForm.Get("grant_code"))
	if code == "" {
		d.oauthFail(w, r, s, fieldErr("Paste the grant code."))
		return
	}
	cfg, err := d.withClient(r, s, pa, scope, info)
	if err != nil {
		d.oauthFail(w, r, s, err)
		return
	}
	raw, _ := json.Marshal(cfg)
	live, err := d.o.Connectors.Get(r.Context(), pa.ID)
	if err != nil {
		d.fail(w, err)
		return
	}
	oc, ok := live.Connector.(connector.OAuthConnector)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.finishConnect(w, r, s, oc, pa, scope, info, raw, "", code, "grant_code")
}

// finishConnect exchanges the code, seals the result into the account's config
// and audits it (never with a secret), then returns to the account page.
func (d *Dashboard) finishConnect(w http.ResponseWriter, r *http.Request, s *session, oc connector.OAuthConnector, pa store.ProviderAccount, scope core.OrgScope,
	info connector.Info, cfg json.RawMessage, redirectURI, code, method string) {
	xctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := oc.OAuthExchange(xctx, cfg, redirectURI, code)
	if err != nil {
		d.renderProvider(w, r, s, http.StatusBadRequest, "Connecting failed: "+err.Error(), "", false, nil)
		return
	}
	var merged map[string]any
	if json.Unmarshal(cfg, &merged) != nil || merged == nil {
		merged = map[string]any{}
	}
	for k, v := range res.ConfigPatch {
		merged[k] = v
	}
	out, _ := json.Marshal(merged)
	if err := d.o.Connectors.UpdateAccount(r.Context(), s.auditActor(), scope, pa.ID, connectors.UpdateAccountInput{Config: out}); err != nil {
		d.oauthFail(w, r, s, err)
		return
	}
	err = d.o.DB.Write(r.Context(), func(tx store.Tx) error {
		return audit.Record(r.Context(), tx, audit.Entry{OrgID: scope.OrgID, Actor: s.auditActor(), Action: "provider_account.oauth_connect", Target: pa.ID,
			After: map[string]any{"connector": pa.Connector, "method": method, "scopes": res.Scopes}})
	})
	if err != nil {
		d.fail(w, err)
		return
	}
	http.Redirect(w, r, prefix+"/providers/"+pa.ID+"?n=oauth_ok", http.StatusSeeOther)
}
