package zoho

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/velixio/paymux/pkg/connector"
)

// Zoho keeps each customer's data in one data centre, and every Zoho host name
// is its service name plus the data centre's domain. This list is the only
// source of such names: a data centre that arrives from config or from an OAuth
// callback is looked up here, never turned into a host by string concatenation,
// so no input can make the connector call a host that is not Zoho's.
var dataCenters = map[string]string{ // data centre -> domain
	"in":     "zoho.in",
	"com":    "zoho.com",
	"eu":     "zoho.eu",
	"com.au": "zoho.com.au",
	"jp":     "zoho.jp",
	"sa":     "zoho.sa",
	"com.cn": "zoho.com.cn",
	"ca":     "zohocloud.ca",
}

// locations are the names Zoho uses for a data centre in the `location`
// parameter of an OAuth callback.
var locations = map[string]string{"us": "com", "eu": "eu", "in": "in", "au": "com.au", "jp": "jp", "ca": "ca", "cn": "com.cn", "sa": "sa"}

// domain is the domain of the config's data centre; ok is false for a data
// centre that is not on the list.
func (c config) domain() (string, bool) {
	d, ok := dataCenters[c.dc()]
	return d, ok
}

func (c config) accountsHost() string {
	d, _ := c.domain()
	return "accounts." + d
}

// defaultScopes are what Paymux needs: payments and refunds, the settings
// (webhook) calls, customers and the payout reads. Zoho's sandbox names the
// same scopes with another prefix.
var defaultScopes = []string{
	"payments.CREATE", "payments.READ", "payments.UPDATE", "refunds.CREATE", "refunds.READ",
	"settings.CREATE", "settings.READ", "settings.UPDATE", "settings.DELETE",
	"customers.CREATE", "customers.READ", "payouts.READ",
}

// oauthConfig reads the config the operator is connecting with.
func oauthConfig(raw json.RawMessage) (config, error) {
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("zoho: invalid config: %w", err)
	}
	if _, ok := cfg.domain(); !ok {
		return cfg, fmt.Errorf("zoho: %q is not a Zoho data centre", cfg.DataCenter)
	}
	return cfg, nil
}

// OAuthDefaultScopes implements connector.OAuthConnector.
func (z *zoho) OAuthDefaultScopes(raw json.RawMessage) []string {
	prefix := "ZohoPay."
	if cfg, err := oauthConfig(raw); err == nil && cfg.Sandbox {
		prefix = "ZohoPaySandbox."
	}
	out := make([]string, len(defaultScopes))
	for i, s := range defaultScopes {
		out[i] = prefix + s
	}
	return out
}

// OAuthAuthorizeURL implements connector.OAuthConnector.
func (z *zoho) OAuthAuthorizeURL(raw json.RawMessage, redirectURI, state string, scopes []string) (string, error) {
	cfg, err := oauthConfig(raw)
	if err != nil {
		return "", err
	}
	switch {
	case cfg.ClientID == "":
		return "", errors.New("zoho: the OAuth client id is needed to connect")
	case state == "":
		return "", errors.New("zoho: an authorization needs a state")
	case redirectURI == "":
		return "", errors.New("zoho: an authorization needs the redirect URI registered with the client")
	}
	q := url.Values{
		"response_type": {"code"}, "access_type": {"offline"}, "prompt": {"consent"},
		"client_id": {cfg.ClientID}, "redirect_uri": {redirectURI}, "scope": {strings.Join(scopes, ",")}, "state": {state},
	}
	// soid names the server the grant is for: zohopaysandbox.<account id> for the
	// sandbox, zohopay.<account id> for the live server (Zoho's sandbox setup and
	// go-live guides). Without it a sandbox account's token is not usable.
	if cfg.AccountID != "" {
		q.Set("soid", map[bool]string{true: "zohopaysandbox.", false: "zohopay."}[cfg.Sandbox]+cfg.AccountID)
	}
	return "https://" + cfg.accountsHost() + "/oauth/v2/auth?" + q.Encode(), nil
}

// OAuthCallbackConfig implements connector.OAuthConnector. After the customer
// approves, Zoho adds `location` (us, eu, in, ...) and `accounts-server` (the
// accounts host of the data centre the account lives in) to the callback: the
// token must be asked for there. Only hosts of the form
// https://accounts.<a Zoho domain> are followed, nothing else.
func (z *zoho) OAuthCallbackConfig(raw json.RawMessage, params url.Values) (json.RawMessage, error) {
	cfg, err := oauthConfig(raw)
	if err != nil {
		return nil, err
	}
	dc := ""
	if server := params.Get("accounts-server"); server != "" {
		if dc = dcOfAccountsServer(server); dc == "" {
			return nil, errors.New("zoho: the callback names an accounts server that is not one of Zoho's")
		}
	} else if loc := strings.ToLower(strings.TrimSpace(params.Get("location"))); loc != "" {
		var ok bool
		if dc, ok = locations[loc]; !ok {
			return nil, errors.New("zoho: the callback names a data centre that is not one of Zoho's")
		}
	}
	if dc == "" {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	_ = cfg
	m["data_center"] = dc
	return json.Marshal(m)
}

// dcOfAccountsServer maps https://accounts.zoho.eu to "eu"; "" when it is not
// exactly an accounts host of a listed domain (no port, user, path or query).
func dcOfAccountsServer(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return ""
	}
	for dc, domain := range dataCenters {
		if strings.EqualFold(u.Host, "accounts."+domain) {
			return dc
		}
	}
	return ""
}

// OAuthExchange implements connector.OAuthConnector.
func (z *zoho) OAuthExchange(ctx context.Context, raw json.RawMessage, redirectURI, code string) (connector.OAuthResult, error) {
	cfg, err := oauthConfig(raw)
	if err != nil {
		return connector.OAuthResult{}, err
	}
	if code == "" {
		return connector.OAuthResult{}, errors.New("zoho: there is no grant code to exchange")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return connector.OAuthResult{}, errors.New("zoho: the OAuth client id and secret are needed to connect")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cfg.ClientID}, "client_secret": {cfg.ClientSecret}, "code": {code}}
	if redirectURI != "" { // a Self Client's code has none
		form.Set("redirect_uri", redirectURI)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+cfg.accountsHost()+"/oauth/v2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return connector.OAuthResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := z.api.http.Do(req)
	if err != nil {
		return connector.OAuthResult{}, transportErr(ctx, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tok struct {
		RefreshToken string          `json:"refresh_token"`
		Scope        string          `json:"scope"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Error        string          `json:"error"`
	}
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode >= 500 {
		return connector.OAuthResult{}, fmt.Errorf("%w: Zoho's accounts server answered HTTP %d", connector.ErrTemporary, resp.StatusCode)
	}
	if tok.Error != "" {
		connector.ReportDecodeError(resp, errors.New("zoho: the token answer is an error: "+safeCode(tok.Error))) // so the refused request reaches the error log
		return connector.OAuthResult{}, fmt.Errorf("zoho: authorization failed (%s); a grant code can be used once and expires in minutes, so authorise again", safeCode(tok.Error))
	}
	if resp.StatusCode != 200 {
		return connector.OAuthResult{}, fmt.Errorf("zoho: authorization failed (HTTP %d)", resp.StatusCode)
	}
	if tok.RefreshToken == "" {
		connector.ReportDecodeError(resp, errors.New("zoho: the token answer has no refresh token"))
		return connector.OAuthResult{}, errors.New("zoho: Zoho returned no refresh token; authorise again (the client must ask for offline access, and a code that was used before returns none)")
	}
	scopes := strings.FieldsFunc(tok.Scope, func(r rune) bool { return r == ',' || r == ' ' })
	exp, _ := strconv.ParseInt(strings.Trim(string(tok.ExpiresIn), `"`), 10, 64)
	return connector.OAuthResult{RefreshToken: tok.RefreshToken, Scopes: scopes, ExpiresIn: exp, ConfigPatch: map[string]any{
		"refresh_token": tok.RefreshToken, "oauth_scopes": strings.Join(scopes, ","), "data_center": cfg.dc(),
	}}, nil
}

// safeCode keeps an error code to short, plain characters, so nothing a server
// sends can reach a log line or a page as anything but a word.
func safeCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == ' ' {
			b.WriteRune(r)
		}
		if b.Len() >= 48 {
			break
		}
	}
	return b.String()
}

var _ connector.OAuthConnector = (*zoho)(nil)
