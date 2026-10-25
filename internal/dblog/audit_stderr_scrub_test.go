package dblog_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Audit 2026-10: the handler passed records to stderr unchanged, so a known
// provider credential echoed in a message, an error, a With attribute or a
// group reached journald/docker logs although the database copy hid it.
func TestAuditKnownSecretsAreScrubbedOnStderr(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, nil)
		const secret = "Kn0wnPr0viderS3cretValue"
		e.red.SetKnown("pa_1", secret)
		var out bytes.Buffer
		l := slog.New(e.rec.Handler(slog.NewJSONHandler(&syncBuf{b: &out}, &slog.HandlerOptions{Level: slog.LevelDebug})))
		l.Warn("provider echoed " + secret)
		l.Warn("x", "err", errors.New("refused key "+secret))
		l.With("detail", "with "+secret).Warn("y")
		l.Warn("z", slog.Group("g", slog.String("v", secret)))
		l.Warn("m", "cfg", map[string]string{"anything": secret})
		if strings.Contains(out.String(), secret) {
			t.Fatalf("a known secret reached stderr:\n%s", out.String())
		}
		if n := strings.Count(out.String(), "\n"); n != 5 {
			t.Fatalf("want 5 records on stderr, got %d:\n%s", n, out.String())
		}
	})
}
