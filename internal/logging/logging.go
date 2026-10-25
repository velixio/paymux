// Package logging builds the Paymux JSON logger with secret and PII redaction.
package logging

import (
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/paymux/paymux/internal/redact"
)

const redacted = "[REDACTED]"

var secretSuffixes = []string{"secret", "password", "token", "authorization", "cookie", "signature", "api_key", "master_key"}

var (
	reURLCreds = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s:/@]+:)[^\s@/]+(@)`)
	rePAN      = regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)
	reGSTIN    = regexp.MustCompile(`\b(\d{2})[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z([0-9A-Z]{1})\b`)
	rePhone    = regexp.MustCompile(`(?:\+|\b)\d(?:[ -]?\d){9,11}\b`)
	reEmail    = regexp.MustCompile(`([A-Za-z0-9._%+-])[A-Za-z0-9._%+-]*@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,})`)
)

// red holds the database log's name and value rules (credential names,
// "Bearer ...", "whsec_...", key=value pairs), so stderr, which every
// deployment keeps, is not the weaker copy. Configured secret values are
// added by the database log handler, which scrubs what it passes on.
var red = redact.New()

// New returns a JSON slog logger writing to w. Values under secret-looking
// keys are replaced, and PII-looking substrings in string values are masked.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	}))
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && (a.Key == slog.TimeKey || a.Key == slog.LevelKey || a.Key == slog.SourceKey) {
		return a
	}
	if isSecretKey(a.Key) || redact.IsCredentialName(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(red.Text(maskString(a.Value.String())))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case nil:
		case error:
			a.Value = slog.StringValue(red.Text(maskString(v.Error())))
		default:
			// A map or struct may hold a credential under any field name.
			if out, ok := red.Value(a.Key, v).(string); ok {
				a.Value = slog.StringValue(maskString(out))
			}
		}
	}
	return a
}

func isSecretKey(k string) bool {
	k = strings.ReplaceAll(strings.ToLower(k), "-", "_")
	for _, s := range secretSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

func maskString(s string) string {
	s = reURLCreds.ReplaceAllString(s, "${1}xxxxx${2}")
	s = rePAN.ReplaceAllStringFunc(s, func(m string) string {
		if !luhnValid(m) {
			return m
		}
		return "[PAN]"
	})
	s = reGSTIN.ReplaceAllStringFunc(s, func(m string) string {
		return m[:2] + strings.Repeat("*", len(m)-5) + m[len(m)-3:]
	})
	s = rePhone.ReplaceAllStringFunc(s, func(m string) string {
		return "******" + m[len(m)-4:]
	})
	s = reEmail.ReplaceAllString(s, "${1}***@${2}")
	return s
}

// luhnValid reports whether the digits in s (separators ignored) pass the
// Luhn check.
func luhnValid(s string) bool {
	sum, alt := 0, false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			continue
		}
		d := int(c - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}
