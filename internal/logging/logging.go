// Package logging builds the Paymux JSON logger with secret and PII redaction.
package logging

import (
	"io"
	"log/slog"
	"regexp"
	"strings"
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

// New returns a JSON slog logger writing to w. Values under secret-looking
// keys are replaced, and PII-looking substrings in string values are masked.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	}))
}

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if isSecretKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(maskString(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok && err != nil {
			a.Value = slog.StringValue(maskString(err.Error()))
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
	s = rePAN.ReplaceAllString(s, "[PAN]")
	s = reGSTIN.ReplaceAllStringFunc(s, func(m string) string {
		return m[:2] + strings.Repeat("*", len(m)-5) + m[len(m)-3:]
	})
	s = rePhone.ReplaceAllStringFunc(s, func(m string) string {
		return "******" + m[len(m)-4:]
	})
	s = reEmail.ReplaceAllString(s, "${1}***@${2}")
	return s
}
