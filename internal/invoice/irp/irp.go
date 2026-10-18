// Package irp verifies the signature of the tokens the GST Invoice
// Registration Portal returns (SignedQRCode and SignedInvoice): JWTs signed
// RS256 with the portal's key.
//
// The check is deliberately narrow. The algorithm must be exactly RS256 (never
// "none", never an HMAC, which would let an attacker sign with the public key
// as the secret); the token must name the key that signed it (kid, x5t or
// x5t#S256) and every name it gives must belong to a key Paymux was given; keys
// the token itself carries or points at (jwk, jku, x5u, x5c) are never used, and
// a token that has them, or "crit", is refused. The keys come only from the
// operator's configuration.
package irp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const minKeyBits = 2048

// Key is a public key the portal's tokens may be signed with, and the names a
// token may give for it.
type Key struct {
	Pub   *rsa.PublicKey
	names map[string]map[string]bool // header field -> accepted values
}

func (k Key) has(field, value string) bool { return k.names[field][value] }

func add(names map[string]map[string]bool, field, value string) {
	if names[field] == nil {
		names[field] = map[string]bool{}
	}
	names[field][value] = true
}

// ParseKeys reads PEM certificates or public keys (RSA, at least 2048 bits).
// A certificate is known by its thumbprints (x5t is the base64url SHA-1 of its
// DER, x5t#S256 the SHA-256) and by the hex of its subject key id and of the
// SHA-1 of its public key; a bare public key by the hex of the SHA-1 of its
// DER encoding. These are the values a token's kid / x5t may carry.
func ParseKeys(pems []string) ([]Key, error) {
	var out []Key
	for i, p := range pems {
		block, _ := pem.Decode([]byte(p))
		if block == nil {
			return nil, fmt.Errorf("irp: key %d is not PEM", i+1)
		}
		k := Key{names: map[string]map[string]bool{}}
		var spki []byte
		switch block.Type {
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("irp: key %d: %w", i+1, err)
			}
			pub, ok := cert.PublicKey.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("irp: key %d is not an RSA key", i+1)
			}
			k.Pub = pub
			s1, s256 := sha1.Sum(cert.Raw), sha256.Sum256(cert.Raw)
			add(k.names, "x5t", base64.RawURLEncoding.EncodeToString(s1[:]))
			add(k.names, "x5t#S256", base64.RawURLEncoding.EncodeToString(s256[:]))
			if len(cert.SubjectKeyId) > 0 {
				add(k.names, "kid", hex.EncodeToString(cert.SubjectKeyId))
			}
			spki = cert.RawSubjectPublicKeyInfo
		case "PUBLIC KEY":
			pub, err := x509.ParsePKIXPublicKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("irp: key %d: %w", i+1, err)
			}
			rk, ok := pub.(*rsa.PublicKey)
			if !ok {
				return nil, fmt.Errorf("irp: key %d is not an RSA key", i+1)
			}
			k.Pub, spki = rk, block.Bytes
		default:
			return nil, fmt.Errorf("irp: key %d is a %q block, not a certificate or public key", i+1, block.Type)
		}
		if k.Pub.N.BitLen() < minKeyBits {
			return nil, fmt.Errorf("irp: key %d has %d bits; at least %d are required", i+1, k.Pub.N.BitLen(), minKeyBits)
		}
		sum := sha1.Sum(spki)
		add(k.names, "kid", hex.EncodeToString(sum[:]))
		out = append(out, k)
	}
	return out, nil
}

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Verify checks token against keys and returns its payload. It fails unless the
// token is a three-part JWT with alg exactly RS256, names a configured key, and
// carries a valid signature by that key.
func Verify(token string, keys []Key) ([]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("irp: no portal key is configured")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, errors.New("irp: a token has three parts separated by dots")
	}
	for _, p := range parts {
		if !segmentRE.MatchString(p) {
			return nil, errors.New("irp: a part of the token is empty or not base64url")
		}
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("irp: the token header is not base64url")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, errors.New("irp: the token header is not a JSON object")
	}
	for _, banned := range []string{"jwk", "jku", "x5u", "crit"} {
		if _, ok := header[banned]; ok {
			return nil, fmt.Errorf("irp: the token header has %q, which Paymux does not accept", banned)
		}
	}
	str := func(field string) (string, bool, error) {
		raw, ok := header[field]
		if !ok {
			return "", false, nil
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return "", true, fmt.Errorf("irp: the token header's %q is not a non-empty string", field)
		}
		return s, true, nil
	}
	alg, ok, err := str("alg")
	if err != nil || !ok || alg != "RS256" {
		return nil, errors.New("irp: the token must be signed with RS256")
	}
	// The token must name its key, and every name must be one of a configured key.
	var matched []Key
	named := false
	for _, field := range []string{"kid", "x5t", "x5t#S256"} {
		v, present, err := str(field)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		named = true
		var next []Key
		for _, k := range keys {
			if k.has(field, v) && (matched == nil || contains(matched, k)) {
				next = append(next, k)
			}
		}
		if len(next) == 0 {
			return nil, fmt.Errorf("irp: the token's %s does not match any configured portal key", field)
		}
		matched = next
	}
	if !named {
		return nil, errors.New("irp: the token names no key (kid or x5t)")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		return nil, errors.New("irp: the token's signature is not readable")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, k := range matched {
		if rsa.VerifyPKCS1v15(k.Pub, crypto.SHA256, digest[:], sig) == nil {
			return base64.RawURLEncoding.DecodeString(parts[1])
		}
	}
	return nil, errors.New("irp: the token's signature is not valid for the portal key it names")
}

func contains(ks []Key, k Key) bool {
	for _, x := range ks {
		if x.Pub == k.Pub {
			return true
		}
	}
	return false
}
