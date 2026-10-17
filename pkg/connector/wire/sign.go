package wire

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standard Webhooks header names and the accepted clock skew.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
	SecretPrefix    = "whsec_"
	Tolerance       = 5 * time.Minute
)

// ErrBadSignature says a message is unsigned, wrongly signed or stale.
var ErrBadSignature = errors.New("wire: missing, wrong or stale signature")

// Signer signs and verifies messages with one shared secret.
type Signer struct{ key []byte }

// NewSigner parses a "whsec_<base64>" secret.
func NewSigner(secret string) (*Signer, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, SecretPrefix))
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("wire: secret must be whsec_ followed by base64: %v", err)
	}
	return &Signer{key: key}, nil
}

func (s *Signer) mac(id string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Sign sets the three headers for body, with a fresh message id.
func (s *Signer) Sign(h http.Header, body []byte, now time.Time) {
	var b [12]byte
	_, _ = rand.Read(b[:])
	id := "msg_" + hex.EncodeToString(b[:])
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	h.Set(HeaderSignature, s.mac(id, now.Unix(), body))
}

// Verify checks the headers against body: all three present, the timestamp
// within Tolerance of now, and one of the space-separated signatures matching.
func (s *Signer) Verify(h http.Header, body []byte, now time.Time) error {
	id, tsRaw, sigs := h.Get(HeaderID), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if id == "" || tsRaw == "" || sigs == "" {
		return ErrBadSignature
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return ErrBadSignature
	}
	want := []byte(s.mac(id, ts, body))
	for _, sig := range strings.Fields(sigs) {
		if hmac.Equal([]byte(sig), want) {
			return nil
		}
	}
	return ErrBadSignature
}
