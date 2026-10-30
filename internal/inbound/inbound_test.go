package inbound_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/inbound"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

type fakeVerifier func(h http.Header, body []byte) (string, error)

func (f fakeVerifier) Verify(h http.Header, body []byte) (string, error) { return f(h, body) }

type env struct {
	t   *testing.T
	db  *store.DB
	h   http.Handler
	acc map[string]inbound.Verifier
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, acc: map[string]inbound.Verifier{}}
	q := jobs.New(db, jobs.Options{Logger: slog.New(slog.DiscardHandler)})
	h := inbound.New(inbound.Options{DB: db, Queue: q, Logger: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return time.UnixMilli(1_800_000_000_000) },
		Verifiers: func(_ context.Context, id string) (inbound.Verifier, error) {
			if id == "pa_boom" {
				return nil, errors.New("db down")
			}
			v, ok := e.acc[id]
			if !ok {
				return nil, inbound.ErrUnknownAccount
			}
			return v, nil
		}})
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{provider_account_id}", h)
	e.h = mux
	return e
}

func (e *env) post(account string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/hooks/"+account, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestVerifiedEventStoredAndQueued(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.acc["pa_1"] = fakeVerifier(func(h http.Header, b []byte) (string, error) { return "ev_1", nil })
		body := []byte(`{"event":"payment.captured"}`)
		rec := e.post("pa_1", body, "X-Sig", "abc", "Authorization", "Bearer secret", "Content-Type", "application/json")
		if rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
		row, err := db.GetProviderEvent(ctx, "pa_1", "ev_1")
		if err != nil {
			t.Fatal(err)
		}
		if !row.Verified || !bytes.Equal(row.Body, body) || row.ReceivedAt.UnixMilli() != 1_800_000_000_000 {
			t.Fatalf("row %+v", row)
		}
		if !strings.Contains(string(row.Headers), "X-Sig") || strings.Contains(string(row.Headers), "secret") ||
			strings.Contains(strings.ToLower(string(row.Headers)), "authorization") {
			t.Fatalf("headers not allow-listed: %s", row.Headers)
		}
		if e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.process'`) != 1 {
			t.Fatal("process job missing")
		}

		// a retry of the same event: 200, no second row or job
		if rec := e.post("pa_1", body); rec.Code != 200 {
			t.Fatalf("dup code %d", rec.Code)
		}
		if e.count(`SELECT COUNT(*) FROM provider_events`) != 1 || e.count(`SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("duplicate delivery created extra rows")
		}
	})
}

func TestEventIDFallsBackToBodyHash(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.acc["pa_1"] = fakeVerifier(func(http.Header, []byte) (string, error) { return "", nil })
		body := []byte(`{"n":1}`)
		if rec := e.post("pa_1", body); rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
		if _, err := db.GetProviderEvent(ctx, "pa_1", sum(body)); err != nil {
			t.Fatalf("no row under sha256(body): %v", err)
		}
	})
}

func TestVerificationFailureStoresUnverifiedAndReturns401(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		good := false
		e.acc["pa_1"] = fakeVerifier(func(http.Header, []byte) (string, error) {
			if good {
				return "ev_1", nil
			}
			return "ev_1", errors.New("bad signature")
		})
		body := []byte(`{"x":1}`)
		if rec := e.post("pa_1", body); rec.Code != 401 {
			t.Fatalf("code %d", rec.Code)
		}
		if e.count(`SELECT COUNT(*) FROM provider_events WHERE verified = 0`) != 1 {
			t.Fatal("unverified event not stored")
		}
		if e.count(`SELECT COUNT(*) FROM jobs`) != 0 {
			t.Fatal("job enqueued for an unverified event")
		}
		// a forged event must not shadow the genuine one with the same id
		good = true
		if rec := e.post("pa_1", body); rec.Code != 200 {
			t.Fatalf("genuine event code %d", rec.Code)
		}
		row, err := db.GetProviderEvent(ctx, "pa_1", "ev_1")
		if err != nil || !row.Verified {
			t.Fatalf("genuine event lost: %+v %v", row, err)
		}
		if e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.process'`) != 1 {
			t.Fatal("genuine event not queued")
		}
	})
}

func TestUnsignedEventIsFetchOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.acc["pa_payu"] = fakeVerifier(func(http.Header, []byte) (string, error) { return "", inbound.ErrUnsigned })
		body := []byte(`status=refunded`)
		if rec := e.post("pa_payu", body); rec.Code != 200 {
			t.Fatalf("code %d", rec.Code)
		}
		row, err := db.GetProviderEvent(ctx, "pa_payu", "unsigned:"+sum(body))
		if err != nil || row.Verified {
			t.Fatalf("row %+v %v", row, err)
		}
		if e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.fetch_only'`) != 1 ||
			e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.process'`) != 0 {
			t.Fatal("expected exactly one fetch_only job")
		}
		e.post("pa_payu", body) // provider retry
		if e.count(`SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("duplicate unsigned delivery queued another job")
		}
	})
}

func TestErrorPaths(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.acc["pa_1"] = fakeVerifier(func(http.Header, []byte) (string, error) { return "ev", nil })
		if rec := e.post("pa_nope", []byte("x")); rec.Code != 404 {
			t.Errorf("unknown account: %d", rec.Code)
		}
		if rec := e.post("pa_boom", []byte("x")); rec.Code != 500 {
			t.Errorf("verifier lookup error: %d", rec.Code)
		}
		if rec := e.post("pa_1", bytes.Repeat([]byte("a"), 1<<20+1)); rec.Code != 413 {
			t.Errorf("oversize: %d", rec.Code)
		}
		if rec := e.post("pa_1", bytes.Repeat([]byte("a"), 1<<20)); rec.Code != 200 {
			t.Errorf("exactly 1 MiB: %d", rec.Code)
		}
		if e.count(`SELECT COUNT(*) FROM provider_events`) != 1 {
			t.Error("rejected requests must not store rows")
		}
	})
}
