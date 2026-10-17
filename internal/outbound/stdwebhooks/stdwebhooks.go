// Package stdwebhooks implements the Standard Webhooks signing scheme
// (https://www.standardwebhooks.com): headers webhook-id, webhook-timestamp
// and webhook-signature: v1,<base64 HMAC-SHA256(id.timestamp.body)>.
package stdwebhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// SecretPrefix prefixes base64 signing secrets.
	SecretPrefix = "whsec_"
	// Tolerance is the accepted clock skew either way.
	Tolerance = 5 * time.Minute

	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

var (
	ErrMissingHeaders = errors.New("stdwebhooks: missing webhook headers")
	ErrTimestamp      = errors.New("stdwebhooks: timestamp invalid or outside tolerance")
	ErrBadSignature   = errors.New("stdwebhooks: no matching signature")
)

// GenerateSecret returns a new random "whsec_<base64>" secret (24 bytes).
func GenerateSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

func decodeSecret(secret string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, SecretPrefix))
	if err != nil {
		return nil, fmt.Errorf("stdwebhooks: secret is not base64: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("stdwebhooks: empty secret")
	}
	return key, nil
}

// Signer signs payloads with one secret.
type Signer struct{ key []byte }

// NewSigner parses a whsec_ secret.
func NewSigner(secret string) (*Signer, error) {
	k, err := decodeSecret(secret)
	if err != nil {
		return nil, err
	}
	return &Signer{key: k}, nil
}

func (s *Signer) mac(id string, ts int64, body []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return m.Sum(nil)
}

// Sign returns the webhook-signature header value ("v1,<base64>").
func (s *Signer) Sign(id string, ts time.Time, body []byte) string {
	return "v1," + base64.StdEncoding.EncodeToString(s.mac(id, ts.Unix(), body))
}

// Headers returns the three signed headers for a message.
func (s *Signer) Headers(id string, ts time.Time, body []byte) http.Header {
	h := http.Header{}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	h.Set(HeaderSignature, s.Sign(id, ts, body))
	return h
}

// Verifier checks incoming Standard Webhooks requests.
type Verifier struct {
	Signer
	Now func() time.Time // default time.Now
}

// NewVerifier parses a whsec_ secret.
func NewVerifier(secret string) (*Verifier, error) {
	s, err := NewSigner(secret)
	if err != nil {
		return nil, err
	}
	return &Verifier{Signer: *s}, nil
}

// Verify checks headers against body and returns the webhook-id. The
// signature header may carry several space-separated signatures; one valid
// v1 signature suffices.
func (v *Verifier) Verify(h http.Header, body []byte) (string, error) {
	id := h.Get(HeaderID)
	tsRaw := h.Get(HeaderTimestamp)
	sigs := h.Get(HeaderSignature)
	if id == "" || tsRaw == "" || sigs == "" {
		return "", ErrMissingHeaders
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return "", ErrTimestamp
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if d := now().Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return "", ErrTimestamp
	}
	want := v.mac(id, ts, body)
	for _, part := range strings.Fields(sigs) {
		ver, val, ok := strings.Cut(part, ",")
		if !ok || ver != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return id, nil
		}
	}
	return "", ErrBadSignature
}
