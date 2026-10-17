package oidc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/oidc"
	"github.com/paymux/paymux/internal/oidc/oidctest"
)

const redirect = "https://pay.example.com/dashboard/login/oidc/callback"

type flow struct {
	idp      *oidctest.IdP
	c        *oidc.Client
	state    string
	nonce    string
	verifier string
}

func newFlow(t *testing.T, mod func(*oidctest.IdP)) *flow {
	t.Helper()
	idp := oidctest.New(t)
	if mod != nil {
		mod(idp)
	}
	return &flow{idp: idp, c: oidc.New(oidc.Config{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret, RedirectURL: redirect, AllowInsecure: true})}
}

// run does the whole round trip for a user and returns the verified claims.
func (f *flow) run(t *testing.T, claims map[string]any) (oidc.Claims, error) {
	t.Helper()
	ctx := context.Background()
	var challenge string
	f.state, f.nonce = oidc.Random(16), oidc.Random(16)
	f.verifier, challenge = oidc.NewPKCE()
	authURL, err := f.c.AuthURL(ctx, f.state, f.nonce, challenge)
	if err != nil {
		t.Fatal(err)
	}
	back := f.idp.Authorize(t, authURL, claims)
	u, _ := url.Parse(back)
	if u.Query().Get("state") != f.state || !strings.HasPrefix(back, redirect) {
		t.Fatalf("callback %s", back)
	}
	raw, err := f.c.Exchange(ctx, u.Query().Get("code"), f.verifier)
	if err != nil {
		return oidc.Claims{}, err
	}
	return f.c.Verify(ctx, raw, f.nonce)
}

var alice = map[string]any{"sub": "u-1", "email": "Alice@Example.com", "email_verified": true, "name": "Alice"}

func TestAuthorizationCodeFlowWithPKCE(t *testing.T) {
	f := newFlow(t, nil)
	cl, err := f.run(t, alice)
	if err != nil || cl.Subject != "u-1" || cl.Email != "alice@example.com" || !cl.EmailVerified || cl.Name != "Alice" {
		t.Fatalf("claims %+v %v", cl, err)
	}
	authURL, _ := f.c.AuthURL(context.Background(), "s", "n", "c")
	u, _ := url.Parse(authURL)
	q := u.Query()
	if !strings.HasPrefix(authURL, f.idp.Issuer+"/authorize?") || q.Get("redirect_uri") != redirect || q.Get("code_challenge_method") != "S256" ||
		q.Get("scope") != "openid email profile" || q.Get("response_type") != "code" {
		t.Errorf("auth URL %s", authURL)
	}
	// The discovery document and the keys are fetched once and reused.
	if _, err := f.run(t, alice); err != nil {
		t.Fatal(err)
	}
	if f.idp.JWKSCalls != 1 {
		t.Errorf("jwks fetched %d times", f.idp.JWKSCalls)
	}
}

func TestPKCEVerifierIsChecked(t *testing.T) {
	f := newFlow(t, nil)
	_, challenge := oidc.NewPKCE()
	authURL, _ := f.c.AuthURL(context.Background(), "s", "n", challenge)
	back, _ := url.Parse(f.idp.Authorize(t, authURL, alice))
	if _, err := f.c.Exchange(context.Background(), back.Query().Get("code"), "not-the-verifier"); !errors.Is(err, oidc.ErrInvalid) {
		t.Fatalf("a code with the wrong verifier was exchanged: %v", err)
	}
	// And a code works once.
	v, ch := oidc.NewPKCE()
	authURL, _ = f.c.AuthURL(context.Background(), "s", "n", ch)
	code := mustCode(t, f.idp.Authorize(t, authURL, alice))
	if _, err := f.c.Exchange(context.Background(), code, v); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Exchange(context.Background(), code, v); err == nil {
		t.Error("an authorization code was accepted twice")
	}
	// A wrong client secret is refused by the IdP.
	bad := oidc.New(oidc.Config{Issuer: f.idp.Issuer, ClientID: f.idp.ClientID, ClientSecret: "wrong", RedirectURL: redirect, AllowInsecure: true})
	v, ch = oidc.NewPKCE()
	authURL, _ = bad.AuthURL(context.Background(), "s", "n", ch)
	if _, err := bad.Exchange(context.Background(), mustCode(t, f.idp.Authorize(t, authURL, alice)), v); !errors.Is(err, oidc.ErrInvalid) {
		t.Errorf("wrong client secret: %v", err)
	}
}

func mustCode(t *testing.T, back string) string {
	t.Helper()
	u, err := url.Parse(back)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("code")
}

func TestIDTokenValidation(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"wrong issuer":       {func(c map[string]any) { c["iss"] = "https://evil.example.com" }, "issuer"},
		"wrong audience":     {func(c map[string]any) { c["aud"] = "someone-else" }, "not for this client"},
		"audience list":      {func(c map[string]any) { c["aud"] = []string{"paymux", "other"} }, "not for this client"}, // no azp
		"expired":            {func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() }, "expired"},
		"not yet valid":      {func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() }, "not valid yet"},
		"issued in future":   {func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() }, "future"},
		"wrong nonce":        {func(c map[string]any) { c["nonce"] = "replayed" }, "nonce"},
		"no nonce":           {func(c map[string]any) { delete(c, "nonce") }, "nonce"},
		"no subject":         {func(c map[string]any) { delete(c, "sub") }, "subject"},
		"no expiry":          {func(c map[string]any) { delete(c, "exp") }, "expired"},
		"foreign authorized": {func(c map[string]any) { c["azp"] = "another-client" }, "not for this client"},
	} {
		f := newFlow(t, func(p *oidctest.IdP) { p.Mutate = tc.mutate })
		_, err := f.run(t, alice)
		if !errors.Is(err, oidc.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Variants that are fine.
	for name, mutate := range map[string]func(map[string]any){
		"audience list with azp": func(c map[string]any) { c["aud"] = []string{"paymux", "other"}; c["azp"] = "paymux" },
		"issuer trailing slash":  func(c map[string]any) { c["iss"] = c["iss"].(string) + "/" },
		"email_verified string":  func(c map[string]any) { c["email_verified"] = "true" },
		"expired within leeway":  func(c map[string]any) { c["exp"] = now.Add(-30 * time.Second).Unix() },
	} {
		f := newFlow(t, func(p *oidctest.IdP) { p.Mutate = mutate })
		if cl, err := f.run(t, alice); err != nil || !cl.EmailVerified {
			t.Errorf("%s: %+v %v", name, cl, err)
		}
	}
	// email_verified false or absent is reported, for the caller to refuse.
	f := newFlow(t, nil)
	cl, err := f.run(t, map[string]any{"sub": "u", "email": "a@example.com"})
	if err != nil || cl.EmailVerified {
		t.Errorf("absent email_verified: %+v %v", cl, err)
	}
}

func TestSignatureAndAlgorithmAttacks(t *testing.T) {
	f := newFlow(t, nil)
	ctx := context.Background()
	good := f.idp.Sign(map[string]any{"iss": f.idp.Issuer, "aud": "paymux", "sub": "u", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "N", "email": "a@example.com"})
	if _, err := f.c.Verify(ctx, good, "N"); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	forged := func(hdr map[string]string, body map[string]any, sig string) string {
		h, _ := json.Marshal(hdr)
		b, _ := json.Marshal(body)
		return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b) + "." + sig
	}
	claims := map[string]any{"iss": f.idp.Issuer, "aud": "paymux", "sub": "u", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "N", "email": "admin@example.com"}
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	for name, tok := range map[string]string{
		"alg none":                            forged(map[string]string{"alg": "none", "kid": "key-1"}, claims, ""),
		"alg none with sig":                   forged(map[string]string{"alg": "none", "kid": "key-1"}, claims, parts[2]),
		"HS256 with the public key as secret": forged(map[string]string{"alg": "HS256", "kid": "key-1"}, claims, parts[2]),
		"payload swapped":                     parts[0] + "." + strings.Split(forged(nil, claims, ""), ".")[1] + "." + parts[2],
		"signed by another key":               oidctest.SignRS256(otherKey, "key-1", claims),
		"unknown kid":                         oidctest.SignRS256(f.idp.Key, "key-2", claims),
		"truncated":                           parts[0] + "." + parts[1],
		"garbage":                             "not.a.jwt",
		"empty":                               "",
		"bad signature b64":                   parts[0] + "." + parts[1] + ".***",
	} {
		if _, err := f.c.Verify(ctx, tok, "N"); !errors.Is(err, oidc.ErrInvalid) {
			t.Errorf("%s accepted or wrong error: %v", name, err)
		}
	}
}

func TestKeyRotationAndRefreshLimit(t *testing.T) {
	now := time.Now()
	clock := &now
	idp := oidctest.New(t)
	c := oidc.New(oidc.Config{Issuer: idp.Issuer, ClientID: "paymux", ClientSecret: "client-secret", RedirectURL: redirect, AllowInsecure: true, Now: func() time.Time { return *clock }})
	ctx := context.Background()
	claims := func() map[string]any {
		return map[string]any{"iss": idp.Issuer, "aud": "paymux", "sub": "u", "exp": now.Add(time.Hour).Unix(), "nonce": "N"}
	}
	if _, err := c.Verify(ctx, idp.Sign(claims()), "N"); err != nil {
		t.Fatal(err)
	}
	// The IdP rotates its key: an unknown kid triggers one refetch and then verifies.
	idp.Rotate(t, "key-2")
	if _, err := c.Verify(ctx, idp.Sign(claims()), "N"); err == nil {
		// within the retry window the refresh is refused, which is fine either way
		t.Log("rotation seen immediately")
	}
	*clock = now.Add(2 * time.Minute)
	if _, err := c.Verify(ctx, idp.Sign(claims()), "N"); err != nil {
		t.Fatalf("after rotation and the retry window: %v", err)
	}
	// Garbage kids must not make the client hammer the IdP.
	calls := idp.JWKSCalls
	for i := 0; i < 20; i++ {
		_, _ = c.Verify(ctx, oidctest.SignRS256(idp.Key, "kid-"+oidc.Random(4), claims()), "N")
	}
	if idp.JWKSCalls-calls > 1 {
		t.Errorf("%d JWKS fetches for unknown kids within a minute", idp.JWKSCalls-calls)
	}
}

func TestES256Tokens(t *testing.T) {
	var jwksHits atomic.Int32
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/a", "token_endpoint": srv.URL + "/t", "jwks_uri": srv.URL + "/jwks"})
		case "/jwks":
			jwksHits.Add(1)
			point, _ := ec.PublicKey.Bytes() // 0x04 || X || Y
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "EC", "crv": "P-256", "kid": "ec1",
				"x": base64.RawURLEncoding.EncodeToString(point[1:33]), "y": base64.RawURLEncoding.EncodeToString(point[33:])}}})
		}
	}))
	defer srv.Close()
	c := oidc.New(oidc.Config{Issuer: srv.URL, ClientID: "paymux", ClientSecret: "s", RedirectURL: redirect, AllowInsecure: true})
	sign := func(key *ecdsa.PrivateKey) string {
		h, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": "ec1"})
		b, _ := json.Marshal(map[string]any{"iss": srv.URL, "aud": "paymux", "sub": "u", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "N"})
		signed := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
		d := sha256.Sum256([]byte(signed))
		r, s, _ := ecdsa.Sign(rand.Reader, key, d[:])
		sig := append(append(make([]byte, 32-len(r.Bytes())), r.Bytes()...), append(make([]byte, 32-len(s.Bytes())), s.Bytes()...)...)
		return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	if _, err := c.Verify(context.Background(), sign(ec), "N"); err != nil {
		t.Fatalf("ES256: %v", err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := c.Verify(context.Background(), sign(other), "N"); !errors.Is(err, oidc.ErrInvalid) {
		t.Errorf("ES256 forged: %v", err)
	}
}

func TestDiscoveryMustNameTheConfiguredIssuer(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://someone-else.example.com", "authorization_endpoint": srv.URL + "/a",
			"token_endpoint": srv.URL + "/t", "jwks_uri": srv.URL + "/j"})
	}))
	defer srv.Close()
	c := oidc.New(oidc.Config{Issuer: srv.URL, ClientID: "x", ClientSecret: "y", RedirectURL: redirect, AllowInsecure: true})
	if _, err := c.AuthURL(context.Background(), "s", "n", "c"); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Errorf("a discovery document for another issuer was accepted: %v", err)
	}
	down := oidc.New(oidc.Config{Issuer: "http://127.0.0.1:1", ClientID: "x", ClientSecret: "y", RedirectURL: redirect, AllowInsecure: true})
	if _, err := down.AuthURL(context.Background(), "s", "n", "c"); err == nil {
		t.Error("an unreachable issuer gave an auth URL")
	}
}
