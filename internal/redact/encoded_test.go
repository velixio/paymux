package redact

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

const encSecret = "Sup3rS3cretSaltValue99"

// leaksRun reports whether out still holds a run of n characters of enc.
func leaksRun(out, enc string, n int) bool {
	for i := 0; i+n <= len(enc); i++ {
		if strings.Contains(out, enc[i:i+n]) {
			return true
		}
	}
	return false
}

// A secret that is logged base64- or hex-encoded (an Authorization header
// rebuilt by hand, a signature input, a debug dump) is still the secret.
func TestEncodedFormsOfKnownSecretsAreRedacted(t *testing.T) {
	r := New()
	r.SetKnown("acct", encSecret)
	raw := []byte(encSecret)
	forms := map[string]string{
		"std padded":     base64.StdEncoding.EncodeToString(raw),
		"std raw":        base64.RawStdEncoding.EncodeToString(raw),
		"url padded":     base64.URLEncoding.EncodeToString(raw),
		"url raw":        base64.RawURLEncoding.EncodeToString(raw),
		"hex lower":      hex.EncodeToString(raw),
		"hex upper":      strings.ToUpper(hex.EncodeToString(raw)),
		"std, 1 shifted": base64.StdEncoding.EncodeToString(append([]byte("x"), raw...)),
		"std, 2 shifted": base64.StdEncoding.EncodeToString(append([]byte("xy"), raw...)),
		"url, 2 shifted": base64.RawURLEncoding.EncodeToString(append([]byte("xy"), raw...)),
	}
	for name, enc := range forms {
		for where, out := range map[string]string{
			"text": r.Text("upstream said " + enc + " was bad"),
			"json": r.Body("application/json", []byte(`{"msg":"bad auth `+enc+`"}`)),
			"url":  r.URL("https://x.test/v1/check/" + enc),
			"err":  r.Text("call: " + enc),
		} {
			if leaksRun(out, enc, 12) {
				t.Errorf("%s in %s: still readable: %s", name, where, out)
			}
		}
	}
}

// The secret inside a longer base64 string, whatever its alignment: Basic
// credentials are base64(keyID:secret), so the secret starts at any offset.
func TestSecretInsideALongerBase64StringIsRedacted(t *testing.T) {
	r := New()
	r.SetKnown("acct", encSecret)
	for pre := 0; pre < 6; pre++ {
		for post := 0; post < 6; post++ {
			plain := strings.Repeat("k", pre) + encSecret + strings.Repeat("z", post)
			for name, enc := range map[string]string{
				"std": base64.StdEncoding.EncodeToString([]byte(plain)), "raw": base64.RawStdEncoding.EncodeToString([]byte(plain)),
				"url": base64.URLEncoding.EncodeToString([]byte(plain)), "rawurl": base64.RawURLEncoding.EncodeToString([]byte(plain)),
			} {
				if out := r.Text("header " + enc); leaksRun(out, enc, 14) {
					t.Errorf("pre=%d post=%d %s: %s", pre, post, name, out)
				}
			}
		}
	}
}

// With the key id known as well, the whole Basic credential goes.
func TestBasicCredentialOfAKnownKeyIDAndSecretIsRedacted(t *testing.T) {
	r := New()
	r.SetKnown("acct", encSecret, "rzp_live_KEYID123")
	cred := "rzp_live_KEYID123:" + encSecret
	for name, enc := range map[string]string{
		"std": base64.StdEncoding.EncodeToString([]byte(cred)), "raw": base64.RawStdEncoding.EncodeToString([]byte(cred)),
		"url": base64.URLEncoding.EncodeToString([]byte(cred)), "rawurl": base64.RawURLEncoding.EncodeToString([]byte(cred)),
	} {
		out := r.Text("auth was " + enc)
		if strings.Contains(out, enc[:20]) || leaksRun(out, enc, 10) {
			t.Errorf("%s: %s", name, out)
		}
	}
}

// Encoded forms must not make ordinary text unreadable.
func TestEncodedVariantsDoNotMangleOrdinaryText(t *testing.T) {
	r := New()
	r.SetKnown("acct", encSecret)
	in := "payment pay_01hzx8k2 captured for order ord-1234, request req_01abc, status=ok"
	if out := r.Text(in); out != in {
		t.Errorf("changed: %s", out)
	}
}
