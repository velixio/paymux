// Package oidctest is an in-process OpenID Connect provider for tests: an
// httptest server with discovery, a JWKS, and an authorization-code token
// endpoint that checks PKCE and the client secret and signs id_tokens.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// IdP is a fake identity provider.
type IdP struct {
	Server       *httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string
	// Key signs id_tokens; KID names it in the JWKS. Tests replace them to
	// simulate a rotation or a forged signature.
	Key *rsa.PrivateKey
	KID string
	// Now stamps iat and exp (default time.Now).
	Now func() time.Time
	// Mutate, when set, edits the id_token claims just before signing.
	Mutate func(claims map[string]any)
	// OmitKey leaves the signing key out of the JWKS.
	OmitKey bool
	// SignWith, when set, signs id_tokens with this key instead of Key (which
	// the JWKS still shows): a forged token.
	SignWith *rsa.PrivateKey

	mu         sync.Mutex
	codes      map[string]grant
	TokenCalls int
	JWKSCalls  int
}

type grant struct {
	claims                       map[string]any
	nonce, challenge, redirectTo string
}

// New starts an IdP for the client "paymux" with the secret "client-secret".
func New(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &IdP{ClientID: "paymux", ClientSecret: "client-secret", Key: key, KID: "key-1", Now: time.Now, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/jwks", p.jwks)
	mux.HandleFunc("/token", p.token)
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	p.Issuer = p.Server.URL
	return p
}

func (p *IdP) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.Issuer, "authorization_endpoint": p.Issuer + "/authorize",
		"token_endpoint": p.Issuer + "/token", "jwks_uri": p.Issuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (p *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	p.JWKSCalls++
	p.mu.Unlock()
	keys := []map[string]any{}
	if !p.OmitKey {
		keys = append(keys, map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": p.KID,
			"n": b64(p.Key.N.Bytes()), "e": b64(big.NewInt(int64(p.Key.E)).Bytes())})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// Authorize plays the user: the browser arrives at authURL (built by the
// client under test), the user signs in as claims (sub, email, ...), and the
// IdP answers with the redirect back to the client, which is returned.
func (p *IdP) Authorize(t testing.TB, authURL string, claims map[string]any) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != p.ClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("the authorization request is not a PKCE code flow with state and nonce: %s", authURL)
	}
	code := b64(randBytes(18))
	p.mu.Lock()
	p.codes[code] = grant{claims: claims, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectTo: q.Get("redirect_uri")}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	return back.String()
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (p *IdP) token(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.TokenCalls++
	p.mu.Unlock()
	fail := func(status int, code string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
		fail(400, "invalid_request")
		return
	}
	if r.PostForm.Get("client_id") != p.ClientID || r.PostForm.Get("client_secret") != p.ClientSecret {
		fail(401, "invalid_client")
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code")) // a code works once
	p.mu.Unlock()
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirectTo {
		fail(400, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if b64(sum[:]) != g.challenge {
		fail(400, "invalid_grant") // PKCE verifier does not match the challenge
		return
	}
	now := p.Now()
	claims := map[string]any{"iss": p.Issuer, "aud": p.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": g.nonce}
	for k, v := range g.claims {
		claims[k] = v
	}
	if p.Mutate != nil {
		p.Mutate(claims)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "unused", "token_type": "Bearer", "id_token": p.sign(claims)})
}

func (p *IdP) sign(claims map[string]any) string {
	if p.SignWith != nil {
		return SignRS256(p.SignWith, p.KID, claims)
	}
	return p.Sign(claims)
}

// Sign makes an RS256 id_token over claims with the IdP's current key.
func (p *IdP) Sign(claims map[string]any) string {
	return SignRS256(p.Key, p.KID, claims)
}

// SignRS256 makes an RS256 JWT with the given key and kid.
func SignRS256(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	body, _ := json.Marshal(claims)
	signed := b64(hdr) + "." + b64(body)
	h := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		panic(fmt.Sprint("oidctest: ", err))
	}
	return signed + "." + b64(sig)
}

// Rotate switches the IdP to a new signing key under a new kid.
func (p *IdP) Rotate(t testing.TB, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.Key, p.KID = key, kid
	p.mu.Unlock()
}
