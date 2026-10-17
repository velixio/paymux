// Package auth provides dashboard credential primitives: argon2id password
// hashing, RFC 6238 TOTP, session tokens and session-bound CSRF tokens.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP: m=19 MiB, t=2, p=1 minimum; we use 64 MiB, t=3).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
	// MinPasswordLen is the minimum dashboard password length.
	MinPasswordLen = 12
)

// ErrWeakPassword is returned for passwords shorter than MinPasswordLen.
var ErrWeakPassword = fmt.Errorf("auth: password must be at least %d characters", MinPasswordLen)

// ErrMalformedHash is returned for unparseable password hashes.
var ErrMalformedHash = errors.New("auth: malformed password hash")

var b64 = base64.RawStdEncoding

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(pw string) (string, error) {
	if len([]rune(pw)) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash produced by HashPassword.
func VerifyPassword(hash, pw string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var v int
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false, ErrMalformedHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m == 0 || t == 0 || p == 0 {
		return false, ErrMalformedHash
	}
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, ErrMalformedHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

const totpStep = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func totpCode(secret []byte, at time.Time, digits int) string {
	return hotp(secret, uint64(at.Unix()/totpStep), digits)
}

func hotp(secret []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, code%mod)
}

// NewTOTPSecret returns a base32 secret and an otpauth:// provisioning URI.
func NewTOTPSecret(issuer, account string) (secret, uri string, err error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	secret = b32.EncodeToString(raw)
	esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	label := esc(issuer) + ":" + esc(account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return secret, "otpauth://totp/" + label + "?" + q.Encode(), nil
}

// ValidateTOTP accepts a 6-digit code for the current step or one step either
// side. lastStep is the last accepted step for this user (-1 if none); codes at
// or before it are rejected to prevent replay. It returns the matched step.
func ValidateTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	raw, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, false
	}
	cur := now.Unix() / totpStep
	for _, s := range []int64{cur - 1, cur, cur + 1} {
		if s <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(raw, uint64(s), 6)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// NewSessionToken returns a random token for the cookie and its SHA-256 hash for storage.
func NewSessionToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken returns the hex-free base64url SHA-256 of a token.
func HashToken(token string) string {
	s := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// CSRFToken derives a CSRF token bound to a session id.
func CSRFToken(key []byte, sessionID string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("csrf:" + sessionID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ValidCSRF checks a CSRF token in constant time.
func ValidCSRF(key []byte, sessionID, token string) bool {
	return hmac.Equal([]byte(CSRFToken(key, sessionID)), []byte(token))
}
