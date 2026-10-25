package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Audit 2026-10: the stderr logger (the one every deployment ships, and the one
// the database log handler wraps) masks only attributes whose key ends in a
// fixed suffix, plus URL passwords, cards, phones and e-mails. Secrets in the
// message, inside error text, under other credential names, or inside a map or
// struct attribute reach stderr unchanged.
func TestAuditStderrLoggerLeaks(t *testing.T) {
	const s = "Zq7pX2mL9vR4tK8w"
	cases := []struct {
		name string
		log  func(l *slog.Logger)
	}{
		{"message kv", func(l *slog.Logger) { l.Error("provider said client_secret=" + s) }},
		{"error text kv", func(l *slog.Logger) { l.Error("x", "err", errors.New("refused: api_key="+s)) }},
		{"bearer in error", func(l *slog.Logger) { l.Error("x", "err", errors.New("Authorization: Bearer "+s)) }},
		{"whsec in string", func(l *slog.Logger) { l.Warn("x", "detail", "secret whsec_"+s) }},
		{"salt key", func(l *slog.Logger) { l.Info("x", "salt", s) }},
		{"private_key key", func(l *slog.Logger) { l.Info("x", "private_key", s) }},
		{"secret_key key", func(l *slog.Logger) { l.Info("x", "secret_key", s) }},
		{"map attr", func(l *slog.Logger) { l.Info("x", "config", map[string]string{"client_secret": s}) }},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		c.log(New(&buf, slog.LevelDebug))
		if strings.Contains(buf.String(), s) {
			t.Errorf("%s: secret reaches stderr: %s", c.name, strings.TrimSpace(buf.String()))
		}
	}
}
