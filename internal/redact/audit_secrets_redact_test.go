package redact

import (
	"strings"
	"testing"
)

// Audit 2026-10: inputs whose secret part survives redaction. Each case names a
// value that must not appear in the output. A failing case is a leak.
func TestAuditRedactLeaks(t *testing.T) {
	const s = "Zq7pX2mL9vR4tK8w" // a secret in no known format, not configured
	r := New()
	cases := []struct {
		name string
		got  string
	}{
		{"text: Authorization Token scheme", r.Text("request failed: Authorization: Token " + s)},
		{"text: Authorization lowercase basic", r.Text("Authorization: basic " + s)},
		{"text: Authorization Digest", r.Text(`Authorization: Digest username="u", response="` + s + `"`)},
		{"text: quoted password with spaces", r.Text(`password="` + s + ` second half"`)},
		{"url: ?key=", r.URL("https://maps.example.com/api?key=" + s)},
		{"url: ?sig=", r.URL("https://blob.example.com/c/f?sv=2024&sig=" + s)},
		{"url: ?auth=", r.URL("https://h.example.com/p?auth=" + s)},
		{"url: ?pass=", r.URL("https://h.example.com/p?user=a&pass=" + s)},
		{"text: error with ?key=", r.Text(`Post "https://api.example.com/v1/charge?key=` + s + `": dial tcp: i/o timeout`)},
		{"json: key", r.Body("application/json", []byte(`{"key":"`+s+`"}`))},
		{"json: pass", r.Body("application/json", []byte(`{"user":"a","pass":"`+s+`"}`))},
		{"json: auth", r.Body("application/json", []byte(`{"auth":"`+s+`"}`))},
		{"json: totp", r.Body("application/json", []byte(`{"totp":"`+s+`"}`))},
		{"form: key", r.Body("application/x-www-form-urlencoded", []byte("merchant=m1&key="+s))},
	}
	for _, c := range cases {
		if strings.Contains(c.got, s) {
			t.Errorf("%s: secret survives: %s", c.name, c.got)
		}
	}
}

// A credential pair after another "label:" is missed: the first pair's value
// regex swallows "password:" and the recursion finds no value after it.
func TestAuditRedactChainedLabel(t *testing.T) {
	const s = "Zq7pX2mL9vR4tK8w"
	for _, in := range []string{
		"upstream error: password: " + s,
		"msg=client_secret: " + s,
	} {
		if got := New().Text(in); strings.Contains(got, s) {
			t.Errorf("secret survives: %q -> %q", in, got)
		}
	}
}
