package oidc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/oidc"
	"github.com/velixio/paymux/pkg/connector"
)

// Discovery, the token endpoint and the JWKS are only reached over https, and
// never at an internal address: the endpoints come from the IdP's own
// document, so a compromised or malicious issuer must not be able to point
// Paymux at the metadata service or an admin port.
func TestEndpointsAreHTTPSAndPublic(t *testing.T) {
	ctx := context.Background()

	// An http issuer is refused before anything is fetched.
	idp := newFlow(t, nil).idp
	plain := oidc.New(oidc.Config{Issuer: idp.Issuer, ClientID: "x", ClientSecret: "y", RedirectURL: redirect})
	if _, err := plain.AuthURL(ctx, "s", "n", "c"); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Errorf("http issuer: %v", err)
	}

	// The default client refuses a name that resolves to an internal address.
	for _, ip := range []string{"10.0.0.9", "169.254.169.254", "127.0.0.1", "192.168.1.1"} {
		c := oidc.New(oidc.Config{Issuer: "https://idp.example.com", ClientID: "x", ClientSecret: "y", RedirectURL: redirect,
			LookupIP: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP(ip)}, nil }})
		if _, err := c.AuthURL(ctx, "s", "n", "c"); !errors.Is(err, connector.ErrPrivateAddress) {
			t.Errorf("issuer resolving to %s: %v", ip, err)
		}
	}

	// A TLS IdP whose discovery document lists a plain-http endpoint is refused.
	for _, field := range []string{"token_endpoint", "jwks_uri", "authorization_endpoint"} {
		var srv *httptest.Server
		srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			doc := map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/a", "token_endpoint": srv.URL + "/t", "jwks_uri": srv.URL + "/j"}
			doc[field] = "http://idp.example.com/insecure"
			_ = json.NewEncoder(w).Encode(doc)
		}))
		c := oidc.New(oidc.Config{Issuer: srv.URL, ClientID: "x", ClientSecret: "y", RedirectURL: redirect, HTTP: srv.Client()})
		if _, err := c.AuthURL(ctx, "s", "n", "c"); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s over http: %v", field, err)
		}
		srv.Close()
	}

	// And one that is fine works over TLS with the default checks.
	var good *httptest.Server
	good = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": good.URL, "authorization_endpoint": good.URL + "/a", "token_endpoint": good.URL + "/t", "jwks_uri": good.URL + "/j"})
	}))
	defer good.Close()
	c := oidc.New(oidc.Config{Issuer: good.URL, ClientID: "x", ClientSecret: "y", RedirectURL: redirect, HTTP: good.Client()})
	if u, err := c.AuthURL(ctx, "s", "n", "c"); err != nil || !strings.HasPrefix(u, good.URL+"/a?") {
		t.Errorf("https IdP: %q %v", u, err)
	}
}
