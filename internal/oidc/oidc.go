// Package oidc is a small OpenID Connect relying party: authorization code flow
// with PKCE, state and nonce, and verification of the id_token signature
// (RS256 or ES256, keys from the issuer's JWKS) and claims. It uses only the
// standard library, so single sign-on adds no dependency.
//
// It is deliberately narrow: discovery, one redirect URI, client_secret_post
// at the token endpoint, and the claims needed to sign someone in (subject,
// email, email_verified). Nothing is kept per user.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Config configures a Client.
type Config struct {
	Issuer       string // the exact `iss` of the id_token, also the discovery base URL
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// HTTP is the client used for discovery, JWKS and the token endpoint. The
	// default has a 10 second timeout, never follows redirects and, unless
	// AllowInsecure is set, refuses loopback, private and link-local addresses
	// (the endpoints come from the IdP's discovery document, so they are not
	// trusted to stay off the internal network).
	HTTP *http.Client
	// AllowInsecure permits http endpoints and private addresses, for an IdP on
	// localhost during development.
	AllowInsecure bool
	// LookupIP replaces the system resolver of the default client (tests).
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// ErrInvalid wraps every failure to authenticate with the IdP's answer: a bad
// token, a mismatch of state or nonce, an unverified e-mail.
var ErrInvalid = errors.New("oidc: invalid sign-in")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

const (
	discoveryTTL = time.Hour
	jwksTTL      = time.Hour
	jwksMinRetry = time.Minute // refetch for an unknown kid at most this often
	leeway       = time.Minute
	maxBody      = 1 << 20
)

// Client talks to one issuer.
type Client struct {
	cfg Config

	dmu    sync.Mutex // guards the discovery document
	disc   *discovery
	discAt time.Time

	kmu         sync.Mutex // guards the signing keys; held across a refresh
	keys        map[string]crypto.PublicKey
	keysAt      time.Time
	keysTriedAt time.Time
}

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// New returns a Client. Nothing is fetched until first use.
func New(cfg Config) *Client {
	if cfg.HTTP == nil {
		cfg.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Timeout: 10 * time.Second, DenyPrivate: !cfg.AllowInsecure, LookupIP: cfg.LookupIP})
		if cfg.AllowInsecure {
			cfg.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	return &Client{cfg: cfg}
}

func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

func (c *Client) discover(ctx context.Context) (*discovery, error) {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if c.disc != nil && c.cfg.Now().Sub(c.discAt) < discoveryTTL {
		return c.disc, nil
	}
	if !c.cfg.AllowInsecure && !strings.HasPrefix(c.cfg.Issuer, "https://") {
		return nil, fmt.Errorf("oidc: the issuer %q is not an https URL", c.cfg.Issuer)
	}
	var d discovery
	if err := c.getJSON(ctx, c.cfg.Issuer+"/.well-known/openid-configuration", &d); err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	// The issuer in the document must be the one configured: otherwise anyone
	// able to serve that URL could name another issuer's endpoints.
	if strings.TrimRight(d.Issuer, "/") != c.cfg.Issuer {
		return nil, fmt.Errorf("oidc: discovery document is for issuer %q, not %q", d.Issuer, c.cfg.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, errors.New("oidc: discovery document lacks an endpoint")
	}
	if !c.cfg.AllowInsecure {
		// The tokens and keys that sign people in only travel over TLS.
		for name, u := range map[string]string{"issuer": c.cfg.Issuer, "authorization_endpoint": d.AuthorizationEndpoint,
			"token_endpoint": d.TokenEndpoint, "jwks_uri": d.JWKSURI} {
			if !strings.HasPrefix(u, "https://") {
				return nil, fmt.Errorf("oidc: the %s %q is not an https URL", name, u)
			}
		}
	}
	c.disc, c.discAt = &d, c.cfg.Now()
	return c.disc, nil
}

// NewPKCE returns a PKCE code verifier and its S256 challenge.
func NewPKCE() (verifier, challenge string) {
	verifier = Random(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// Random returns n random bytes, URL-safe base64 encoded (for state, nonce and
// verifiers).
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("oidc: entropy source failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL is where to send the browser to sign in.
func (c *Client) AuthURL(ctx context.Context, state, nonce, challenge string) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("oidc: bad authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange trades the authorization code for the raw id_token.
func (c *Client) Exchange(ctx context.Context, code, verifier string) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.cfg.RedirectURL},
		"client_id": {c.cfg.ClientID}, "client_secret": {c.cfg.ClientSecret}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc: token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode != http.StatusOK || tok.IDToken == "" {
		// The IdP's error code is safe to report; the body may not be.
		return "", invalid("token endpoint answered HTTP %d %q", resp.StatusCode, tok.Error)
	}
	return tok.IDToken, nil
}

// Claims are the id_token claims Paymux uses.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Verify checks the id_token's signature and claims and returns them. nonce is
// the value sent with the authorization request.
func (c *Client) Verify(ctx context.Context, raw, nonce string) (Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Claims{}, invalid("id_token is not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	if err := decodePart(parts[0], &hdr); err != nil {
		return Claims{}, invalid("id_token header: %v", err)
	}
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" { // never "none", never an HMAC with a public key
		return Claims{}, invalid("unsupported id_token algorithm %q", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, invalid("id_token signature: %v", err)
	}
	key, err := c.key(ctx, hdr.KID, hdr.Alg)
	if err != nil {
		return Claims{}, err
	}
	if err := verifySignature(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return Claims{}, invalid("id_token signature does not verify")
	}

	var cl struct {
		Iss           string          `json:"iss"`
		Sub           string          `json:"sub"`
		Aud           json.RawMessage `json:"aud"`
		Azp           string          `json:"azp"`
		Exp           float64         `json:"exp"`
		Nbf           float64         `json:"nbf"`
		Iat           float64         `json:"iat"`
		Nonce         string          `json:"nonce"`
		Email         string          `json:"email"`
		EmailVerified any             `json:"email_verified"`
		Name          string          `json:"name"`
	}
	if err := decodePart(parts[1], &cl); err != nil {
		return Claims{}, invalid("id_token claims: %v", err)
	}
	now := c.cfg.Now()
	switch {
	case strings.TrimRight(cl.Iss, "/") != c.cfg.Issuer:
		return Claims{}, invalid("issuer %q is not %q", cl.Iss, c.cfg.Issuer)
	case !audienceOK(cl.Aud, c.cfg.ClientID, cl.Azp):
		return Claims{}, invalid("the id_token is not for this client")
	case cl.Exp == 0 || !now.Before(time.Unix(int64(cl.Exp), 0).Add(leeway)):
		return Claims{}, invalid("the id_token has expired")
	case cl.Nbf != 0 && now.Add(leeway).Before(time.Unix(int64(cl.Nbf), 0)):
		return Claims{}, invalid("the id_token is not valid yet")
	case cl.Iat != 0 && now.Add(5*time.Minute).Before(time.Unix(int64(cl.Iat), 0)):
		return Claims{}, invalid("the id_token was issued in the future")
	case nonce == "" || subtle.ConstantTimeCompare([]byte(cl.Nonce), []byte(nonce)) != 1:
		return Claims{}, invalid("nonce mismatch")
	case cl.Sub == "":
		return Claims{}, invalid("the id_token has no subject")
	}
	verified := false
	switch v := cl.EmailVerified.(type) { // some IdPs send the string "true"
	case bool:
		verified = v
	case string:
		verified = v == "true"
	}
	return Claims{Subject: cl.Sub, Email: strings.ToLower(strings.TrimSpace(cl.Email)), EmailVerified: verified, Name: cl.Name}, nil
}

func audienceOK(raw json.RawMessage, clientID, azp string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == clientID && (azp == "" || azp == clientID)
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	found := false
	for _, a := range many {
		found = found || a == clientID
	}
	return found && (len(many) == 1 && azp == "" || azp == clientID)
}

func decodePart(p string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func verifySignature(alg string, key crypto.PublicKey, signed, sig []byte) error {
	h := sha256.Sum256(signed)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if alg != "RS256" {
			return errors.New("key/alg mismatch")
		}
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig)
	case *ecdsa.PublicKey:
		if alg != "ES256" || len(sig) != 64 {
			return errors.New("key/alg mismatch")
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(k, h[:], r, s) {
			return errors.New("bad signature")
		}
		return nil
	}
	return errors.New("unsupported key")
}

// key returns the verification key for kid, refreshing the JWKS when it is
// stale or the kid is unknown (a key rotation), at most once a minute.
func (c *Client) key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	c.kmu.Lock()
	defer c.kmu.Unlock()
	now := c.cfg.Now()
	pick := func() (crypto.PublicKey, bool) {
		if k, ok := c.keys[kid]; ok {
			return k, true
		}
		if kid == "" && len(c.keys) == 1 {
			for _, k := range c.keys {
				return k, true
			}
		}
		return nil, false
	}
	if c.keys != nil && now.Sub(c.keysAt) < jwksTTL {
		if k, ok := pick(); ok {
			return k, nil
		}
	}
	if c.keys != nil && now.Sub(c.keysTriedAt) < jwksMinRetry {
		return nil, invalid("unknown signing key %q", kid)
	}
	c.keysTriedAt = now
	d, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err = c.getJSON(ctx, d.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("oidc: fetching signing keys: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if pk, ok := k.public(); ok {
			keys[k.KID] = pk
		}
	}
	c.keys, c.keysAt = keys, now
	if k, ok := pick(); ok {
		return k, nil
	}
	return nil, invalid("unknown signing key %q", kid)
}

type jwk struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) public() (crypto.PublicKey, bool) {
	if k.Use != "" && k.Use != "sig" {
		return nil, false
	}
	b := func(s string) *big.Int {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(raw) == 0 {
			return nil
		}
		return new(big.Int).SetBytes(raw)
	}
	switch k.KTY {
	case "RSA":
		n, e := b(k.N), b(k.E)
		if n == nil || e == nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || n.BitLen() < 2048 {
			return nil, false
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, true
	case "EC":
		x, y := b(k.X), b(k.Y)
		if k.Crv != "P-256" || x == nil || y == nil || x.BitLen() > 256 || y.BitLen() > 256 {
			return nil, false
		}
		point := make([]byte, 65) // 0x04 || X || Y, which also checks the point is on the curve
		point[0] = 4
		x.FillBytes(point[1:33])
		y.FillBytes(point[33:])
		pk, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		return pk, err == nil
	}
	return nil, false
}
