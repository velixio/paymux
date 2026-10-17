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

// mac signs the message id, the timestamp, a binding string and the body. The
// binding fixes what the message is for (see requestBinding), so a signed
// message cannot be replayed against another method.
func (s *Signer) mac(id string, ts int64, binding string, body []byte) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "." + binding + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// requestBinding is the part of a request, besides the body, the signature
// covers: the HTTP verb and the method name, as in "POST.refund".
func requestBinding(verb, method string) string { return verb + "." + method }

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// SignRequest sets the three headers of a request for verb and method, with a
// fresh message id (returned: a response is bound to it).
func (s *Signer) SignRequest(h http.Header, verb, method string, body []byte, now time.Time) string {
	id := newID()
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	h.Set(HeaderSignature, s.mac(id, now.Unix(), requestBinding(verb, method), body))
	return id
}

// VerifyRequest checks a request for verb and method (the method taken from
// the URL path, which the signature covers): all three headers present, the
// timestamp within Tolerance of now, and one of the space-separated signatures
// matching.
func (s *Signer) VerifyRequest(h http.Header, verb, method string, body []byte, now time.Time) error {
	return s.verify(h, requestBinding(verb, method), body, now)
}

// responseBinding ties a response to the request it answers; the "response"
// label keeps a response signature from ever passing as a request signature.
func responseBinding(requestID string) string { return "response." + requestID }

// SignResponse sets the three headers of a response to the request with the
// given message id: the signature covers that id and the body.
func (s *Signer) SignResponse(h http.Header, requestID string, body []byte, now time.Time) {
	id := newID()
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	h.Set(HeaderSignature, s.mac(id, now.Unix(), responseBinding(requestID), body))
}

// VerifyResponse checks a response to the request with the given message id,
// so a response recorded for one call cannot be handed back for another.
func (s *Signer) VerifyResponse(h http.Header, requestID string, body []byte, now time.Time) error {
	return s.verify(h, responseBinding(requestID), body, now)
}

func (s *Signer) verify(h http.Header, binding string, body []byte, now time.Time) error {
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
	want := []byte(s.mac(id, ts, binding, body))
	for _, sig := range strings.Fields(sigs) {
		if hmac.Equal([]byte(sig), want) {
			return nil
		}
	}
	return ErrBadSignature
}
