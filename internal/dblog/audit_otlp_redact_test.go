package dblog_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/dblog"
	"github.com/paymux/paymux/internal/redact"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Audit R-A10: what goes to the collector is redacted like the database row.

// A known secret, logged in every way a record can carry it, never reaches the
// OTLP request, also in the forms the redactor knows (base64, hex, URL-escaped).
func TestAuditOTLPPayloadNeverCarriesAKnownSecret(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		const secret = "Kn0wnPr0viderS3cretValue"
		sink := &otlpSink{}
		srv := httptest.NewServer(sink)
		defer srv.Close()
		o := dblog.NewOTLP(dblog.OTLPOptions{Endpoint: srv.URL, FlushEvery: 5 * time.Millisecond, Stdout: slog.New(slog.DiscardHandler)})
		o.Start()
		defer o.Close()
		e := newEnv(t, db, func(op *dblog.Options) { op.Tee = o.Ship })
		e.red.SetKnown("pa_1", secret)
		e.log.Warn("provider echoed "+secret, "detail", "with "+secret, "err", errors.New("refused key "+secret))
		e.log.Warn("encoded", "b64", base64.StdEncoding.EncodeToString([]byte(secret)), "hex", hex.EncodeToString([]byte(secret)))
		e.log.With("attached", secret).Warn("via With", slog.Group("g", slog.String("v", secret)))
		e.log.Warn("map", "cfg", map[string]string{"anything": secret})
		e.rec.Flush(context.Background())
		sink.wait(t, 1)
		time.Sleep(50 * time.Millisecond)
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for _, b := range sink.bodies {
			for _, form := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), hex.EncodeToString([]byte(secret))} {
				if strings.Contains(string(b), form) {
					t.Fatalf("a known secret (%q) reached the OTLP payload:\n%s", form, b)
				}
			}
		}
	})
}

// A secret that becomes known after a row was built (a provider account saved
// while the row waits in the queue) is still removed when the batch is sent.
func TestAuditOTLPRedactsAgainWhenSending(t *testing.T) {
	const secret = "Late5ecretV4lue"
	sink := &otlpSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()
	red := redact.New()
	o := dblog.NewOTLP(dblog.OTLPOptions{Endpoint: srv.URL, FlushEvery: 5 * time.Millisecond, Redact: red.Scrub, Stdout: slog.New(slog.DiscardHandler)})
	o.Start()
	defer o.Close()
	red.SetKnown("pa_late", secret)
	o.Ship([]store.LogRow{{ID: "log_1", At: time.Now(), Level: "error", Kind: "provider", Component: "c", Msg: "echo " + secret,
		Attrs: `{"k":"` + secret + `"}`, Request: `{"body":"` + secret + `"}`, Response: `{"body":"` + secret + `"}`}})
	sink.wait(t, 1)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if strings.Contains(string(sink.bodies[0]), secret) {
		t.Fatalf("a secret learned after the row was built reached the collector:\n%s", sink.bodies[0])
	}
	if !strings.Contains(string(sink.bodies[0]), "log_1") {
		t.Errorf("the record itself is missing:\n%s", sink.bodies[0])
	}
}
