package connectortest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// oauthMock is the conformant mock plus the OAuth capability.
type oauthMock struct{ mock }

func (m *oauthMock) Info() connector.Info {
	i := m.mock.Info()
	i.Capabilities = []connector.Capability{connector.CapOAuth}
	return i
}

func (m *oauthMock) OAuthDefaultScopes(json.RawMessage) []string {
	return []string{"pay.read", "pay.write"}
}

func (m *oauthMock) OAuthAuthorizeURL(cfg json.RawMessage, redirectURI, state string, scopes []string) (string, error) {
	if state == "" || redirectURI == "" {
		return "", errors.New("state and redirect URI are required")
	}
	q := url.Values{"client_id": {"c"}, "redirect_uri": {redirectURI}, "state": {state}, "scope": {strings.Join(scopes, ",")}}
	return "https://accounts.example.com/oauth/auth?" + q.Encode(), nil
}

func (m *oauthMock) OAuthCallbackConfig(cfg json.RawMessage, _ url.Values) (json.RawMessage, error) {
	return cfg, nil
}

func (m *oauthMock) OAuthExchange(ctx context.Context, _ json.RawMessage, redirectURI, code string) (connector.OAuthResult, error) {
	if code == "" {
		return connector.OAuthResult{}, errors.New("no code")
	}
	resp, err := m.http.Get("https://accounts.example.com/oauth/token?code=" + url.QueryEscape(code))
	if err != nil {
		return connector.OAuthResult{}, err
	}
	defer resp.Body.Close()
	var v struct {
		Refresh string `json:"refresh_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if v.Refresh == "" {
		return connector.OAuthResult{}, errors.New("no refresh token")
	}
	return connector.OAuthResult{RefreshToken: v.Refresh, Scopes: []string{"pay.read"}, ExpiresIn: 3600}, nil
}

func TestKitChecksTheOAuthCapability(t *testing.T) {
	factory := func(_ json.RawMessage, d connector.Deps) (connector.Connector, error) {
		return &oauthMock{mock{secret: "s3cret", http: d.HTTP}}, nil
	}
	body := []byte(`{"id":1}`)
	m := &mock{secret: "s3cret"}
	wrong := &mock{secret: "other"}
	connectortest.Run(t, factory, connectortest.Fixtures{
		Config: json.RawMessage(`{}`),
		Ref:    connector.PaymentRef{ProviderPaymentID: "p1"},
		Statuses: []connectortest.StatusCase{
			{Raw: "ok", Want: connector.PaymentSucceeded, WantAmount: 500, Handler: statusHandler("ok")},
			{Raw: "bad", Want: connector.PaymentFailed, WantAmount: 500, Handler: statusHandler("bad")},
		},
		Webhooks: connectortest.WebhookFixtures{
			Valid:            connector.WebhookRequest{Method: "POST", Body: body, Headers: map[string][]string{"X-Sig": {m.sign(body)}}},
			WrongSecret:      connector.WebhookRequest{Method: "POST", Body: body, Headers: map[string][]string{"X-Sig": {wrong.sign(body)}}},
			SignatureHeaders: []string{"X-Sig"},
		},
		Return: &connectortest.ReturnFixtures{
			Valid:    connector.ReturnRequest{ProviderRef: "r", Query: map[string][]string{"sig": {"good"}}},
			Tampered: connector.ReturnRequest{ProviderRef: "r", Query: map[string][]string{"sig": {"bad"}}},
			Want:     connector.ReturnResult{ProviderRef: "r", ProviderPaymentID: "p1"},
		},
		Checkout: connector.CheckoutRequest{ProviderRef: "r1", Amount: 100, Currency: "INR"},
		OAuth: &connectortest.OAuthFixtures{
			RedirectURI: "https://pay.example.com/dashboard/oauth/callback",
			Code:        "grant-code",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"refresh_token": "rt_1", "access_token": "at_1"})
			}),
		},
	})
}
