package auth

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	if ok, _ := VerifyPassword(h, "correct horse battery staple"); !ok {
		t.Fatal("valid password rejected")
	}
	if ok, _ := VerifyPassword(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Fatal("salt not random")
	}
	if _, err := VerifyPassword("$argon2id$garbage", "x"); err == nil {
		t.Fatal("malformed hash accepted")
	}
}

func TestPasswordPolicy(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("password under 12 chars accepted")
	}
}

// RFC 6238 Appendix B vectors (SHA1, 8 digits) for secret "12345678901234567890".
func TestTOTPRFCVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	vectors := map[int64]string{
		59:          "94287082",
		1111111109:  "07081804",
		1111111111:  "14050471",
		1234567890:  "89005924",
		2000000000:  "69279037",
		20000000000: "65353130",
	}
	for ts, want := range vectors {
		if got := totpCode(secret, time.Unix(ts, 0), 8); got != want {
			t.Errorf("t=%d got %s want %s", ts, got, want)
		}
	}
}

func TestTOTPValidateWindowAndReplay(t *testing.T) {
	secret, uri, err := NewTOTPSecret("Paymux", "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "otpauth://totp/Paymux:ops%40example.com?") || !strings.Contains(uri, "secret=") {
		t.Fatalf("uri %q", uri)
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1760700000, 0)
	code := totpCode(raw, now, 6)
	step, ok := ValidateTOTP(secret, code, now, -1)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := ValidateTOTP(secret, code, now, step); ok {
		t.Fatal("replayed code accepted")
	}
	prev := totpCode(raw, now.Add(-30*time.Second), 6)
	if _, ok := ValidateTOTP(secret, prev, now, -1); !ok {
		t.Fatal("previous step (clock skew) rejected")
	}
	old := totpCode(raw, now.Add(-90*time.Second), 6)
	if _, ok := ValidateTOTP(secret, old, now, -1); ok {
		t.Fatal("code 3 steps old accepted")
	}
	if _, ok := ValidateTOTP(secret, "12345", now, -1); ok {
		t.Fatal("malformed code accepted")
	}
}

func TestTokens(t *testing.T) {
	tok, hash := NewSessionToken()
	if len(tok) < 40 || hash == tok || HashToken(tok) != hash {
		t.Fatalf("token %q hash %q", tok, hash)
	}
	a, _ := NewSessionToken()
	if a == tok {
		t.Fatal("tokens not random")
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	c := CSRFToken(key, "sess_1")
	if !ValidCSRF(key, "sess_1", c) || ValidCSRF(key, "sess_2", c) || ValidCSRF(key, "sess_1", c+"x") {
		t.Fatal("csrf binding broken")
	}
}
