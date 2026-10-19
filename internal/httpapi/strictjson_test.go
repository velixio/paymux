package httpapi_test

import (
	"strings"
	"testing"
)

// {"amount":1,"amount":2} is read differently by different parsers (first wins,
// last wins); a body that says the same thing twice is refused outright.
func TestDuplicateJSONKeysAreRefused(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		for name, body := range map[string]string{
			"top level":             `{"amount":100,"amount":1,"currency":"INR"}`,
			"nested":                `{"amount":100,"currency":"INR","metadata":{"a":"1","a":"2"}}`,
			"in the customer":       `{"amount":100,"currency":"INR","customer":{"email":"a@example.com","email":"b@example.com"}}`,
			"repeated string field": `{"amount":100,"currency":"INR","order_ref":"a","order_ref":"b"}`,
		} {
			r := f.do("POST", "/v1/payments", f.testKey, body)
			if r.status != 400 || r.errType(t) != "invalid_request" || !strings.Contains(string(r.raw), "duplicate") {
				t.Errorf("%s: %d %s", name, r.status, r.raw)
			}
		}
		if n := f.count("SELECT COUNT(*) FROM payments"); n != 0 {
			t.Errorf("%d payments were created from refused bodies", n)
		}
		// Distinct keys are fine.
		ok := `{"amount":100,"currency":"INR","metadata":{"a":"1"},"customer":{"email":"a@example.com"}}`
		if r := f.do("POST", "/v1/payments", f.testKey, ok); r.status != 201 {
			t.Errorf("a valid body: %d %s", r.status, r.raw)
		}
	})
}

// The reply says what is wrong with the request in the API's terms, not in the
// words of the Go decoder (type and package names, struct paths).
func TestBadBodiesDoNotEchoTheDecoderError(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		for name, tc := range map[string]struct{ body, want string }{
			"wrong type":    {`{"amount":"lots","currency":"INR"}`, `amount`},
			"unknown field": {`{"amount":100,"currency":"INR","bogus":1}`, `bogus`},
			"syntax":        {`{"amount":100,`, "JSON"},
			"trailing":      {`{"amount":100,"currency":"INR"} {}`, "JSON"},
			"not an object": {`[1,2]`, "JSON"},
		} {
			r := f.do("POST", "/v1/payments", f.testKey, tc.body)
			msg := string(r.raw)
			if r.status != 400 || !strings.Contains(msg, tc.want) {
				t.Errorf("%s: %d %s (want %s)", name, r.status, msg, tc.want)
			}
			for _, leak := range []string{"Go struct", "Go value", "cannot unmarshal", "json:", "httpapi.", "core.", "unexpected end", "invalid character"} {
				if strings.Contains(msg, leak) {
					t.Errorf("%s: the reply leaks %q: %s", name, leak, msg)
				}
			}
		}
	})
}
