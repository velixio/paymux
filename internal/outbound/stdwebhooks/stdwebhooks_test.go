package stdwebhooks

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Vector from the Standard Webhooks reference libraries' test suites
// (recalled from upstream; network fetch was unavailable and the signature
// was independently recomputed).
const (
	vecSecret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vecID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vecTS     = int64(1614265330)
	vecBody   = `{"test": 2432232314}`
	vecSig    = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func TestSignVector(t *testing.T) {
	s, err := NewSigner(vecSecret)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Sign(vecID, time.Unix(vecTS, 0), []byte(vecBody))
	if got != vecSig {
		t.Fatalf("got %q want %q", got, vecSig)
	}
}

func hdr(id string, ts int64, sig string) http.Header {
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	h.Set("webhook-signature", sig)
	return h
}

func TestVerify(t *testing.T) {
	now := time.Unix(vecTS, 0)
	v, _ := NewVerifier(vecSecret)
	v.Now = func() time.Time { return now }
	body := []byte(vecBody)

	if _, err := v.Verify(hdr(vecID, vecTS, vecSig), body); err != nil {
		t.Fatalf("valid: %v", err)
	}
	// multiple signatures, space separated, one valid
	if _, err := v.Verify(hdr(vecID, vecTS, "v1,AAAA "+vecSig+" v2,zzzz"), body); err != nil {
		t.Fatalf("multi: %v", err)
	}
	// skew tolerated at 5m, rejected beyond
	v.Now = func() time.Time { return now.Add(5 * time.Minute) }
	if _, err := v.Verify(hdr(vecID, vecTS, vecSig), body); err != nil {
		t.Fatalf("5m skew: %v", err)
	}
	v.Now = func() time.Time { return now.Add(5*time.Minute + time.Second) }
	if _, err := v.Verify(hdr(vecID, vecTS, vecSig), body); !errors.Is(err, ErrTimestamp) {
		t.Fatalf("old: %v", err)
	}
	v.Now = func() time.Time { return now.Add(-5*time.Minute - time.Second) }
	if _, err := v.Verify(hdr(vecID, vecTS, vecSig), body); !errors.Is(err, ErrTimestamp) {
		t.Fatalf("future: %v", err)
	}
	v.Now = func() time.Time { return now }

	badTS := hdr(vecID, vecTS, vecSig)
	badTS.Set("webhook-timestamp", "x")
	cases := map[string]struct {
		h    http.Header
		body []byte
		want error
	}{
		"missing headers": {http.Header{}, body, ErrMissingHeaders},
		"bad ts":          {badTS, body, ErrTimestamp},
		"tampered body":   {hdr(vecID, vecTS, vecSig), []byte(`{"test": 1}`), ErrBadSignature},
		"wrong id":        {hdr("msg_other", vecTS, vecSig), body, ErrBadSignature},
		"no v1":           {hdr(vecID, vecTS, "v2,"+strings.TrimPrefix(vecSig, "v1,")), body, ErrBadSignature},
		"garbage sig":     {hdr(vecID, vecTS, "v1,!!!"), body, ErrBadSignature},
	}
	for name, c := range cases {
		if _, err := v.Verify(c.h, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
}

func TestVerifyReturnsEventID(t *testing.T) {
	v, _ := NewVerifier(vecSecret)
	v.Now = func() time.Time { return time.Unix(vecTS, 0) }
	id, err := v.Verify(hdr(vecID, vecTS, vecSig), []byte(vecBody))
	if err != nil || id != vecID {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestSecretFormats(t *testing.T) {
	if _, err := NewSigner("whsec_!!!"); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := NewSigner("whsec_"); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := NewSigner("MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"); err != nil {
		t.Fatal(err)
	}
	s, err := GenerateSecret()
	if err != nil || !strings.HasPrefix(s, "whsec_") {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := NewSigner(s); err != nil {
		t.Fatal(err)
	}
}
