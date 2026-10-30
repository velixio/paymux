package inbound_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func (e *env) postFrom(ip, account string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest("POST", "/hooks/"+account, bytes.NewReader(body))
	req.RemoteAddr = ip + ":4000"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func badAndGood(e *env, account string) *bool {
	good := false
	e.acc[account] = fakeVerifier(func(h http.Header, b []byte) (string, error) {
		if good {
			return "ev_" + sum(b)[:8], nil
		}
		return "", errors.New("bad signature")
	})
	return &good
}

// An event that fails verification is evidence, not data: only its hash, size
// and a short prefix are kept, not up to a mebibyte of attacker-chosen bytes.
func TestFailedVerificationKeepsOnlyAHashSizeAndPrefix(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		badAndGood(e, "pa_1")
		body := append([]byte(`{"hello":"world"}`), bytes.Repeat([]byte("x"), 1<<20-20)...)
		if rec := e.post("pa_1", body); rec.Code != 401 {
			t.Fatalf("code %d", rec.Code)
		}
		row, err := db.GetProviderEvent(ctx, "pa_1", "unverified:"+sum(body))
		if err != nil {
			t.Fatal(err)
		}
		if len(row.Body) > 256 || !bytes.HasPrefix(row.Body, []byte(`{"hello":"world"}`)) {
			t.Errorf("body kept: %d bytes", len(row.Body))
		}
		if !strings.Contains(row.Error, fmt.Sprintf("size=%d", len(body))) || !strings.Contains(row.Error, "sha256="+sum(body)) {
			t.Errorf("error = %q", row.Error)
		}
		if row.Verified {
			t.Error("verified")
		}
	})
}

// An event the provider cannot sign is stored under its own key, so it can
// never take the place of a signed event with the same id; the key includes the
// body, so a replayed id with another body is another event.
func TestUnsignedEventsHaveTheirOwnKeys(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		signed := false
		e.acc["pa_1"] = fakeVerifier(func(h http.Header, b []byte) (string, error) {
			if signed {
				return "ev_9", nil
			}
			return "ev_9", inbound.ErrUnsigned
		})
		first, second := []byte(`{"n":1}`), []byte(`{"n":2}`)
		e.post("pa_1", first)
		e.post("pa_1", first)  // the provider's retry: the same event
		e.post("pa_1", second) // same id, another body: another event
		if n := e.count(`SELECT COUNT(*) FROM provider_events WHERE provider_event_id LIKE 'unsigned:ev_9%'`); n != 2 {
			t.Fatalf("unsigned rows = %d, want 2", n)
		}
		if e.count(`SELECT COUNT(*) FROM provider_events WHERE provider_event_id = 'ev_9'`) != 0 {
			t.Fatal("an unsigned event took the signed event's key")
		}
		signed = true
		if rec := e.post("pa_1", first); rec.Code != 200 {
			t.Fatalf("signed delivery: %d", rec.Code)
		}
		row, err := db.GetProviderEvent(ctx, "pa_1", "ev_9")
		if err != nil || !row.Verified {
			t.Fatalf("the signed event was shadowed: %+v %v", row, err)
		}
		if e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.process'`) != 1 {
			t.Error("the signed event was not queued")
		}
	})
}

func newLimited(t *testing.T, db *store.DB, mut func(*inbound.Options)) *env {
	t.Helper()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, acc: map[string]inbound.Verifier{}}
	q := jobs.New(db, jobs.Options{Logger: slog.New(slog.DiscardHandler)})
	o := inbound.Options{DB: db, Queue: q, Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return time.UnixMilli(1_800_000_000_000) },
		Verifiers: func(_ context.Context, id string) (inbound.Verifier, error) {
			v, ok := e.acc[id]
			if !ok {
				return nil, inbound.ErrUnknownAccount
			}
			return v, nil
		}}
	if mut != nil {
		mut(&o)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /hooks/{provider_account_id}", inbound.New(o))
	e.h = mux
	return e
}

// A flood of forged events from one address stores a bounded number of rows and
// does not stop genuine events, from that address or any other.
func TestForgedEventFloodIsBounded(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newLimited(t, db, func(o *inbound.Options) { o.FailuresPerIP, o.FailuresPerAccount, o.RequestsPerIP = 20, 100, 100000 })
		good := badAndGood(e, "pa_1")
		for i := 0; i < 2000; i++ {
			rec := e.postFrom("203.0.113.9", "pa_1", []byte(fmt.Sprintf(`{"forged":%d}`, i)))
			if rec.Code != 401 && rec.Code != 429 {
				t.Fatalf("forged event %d: %d", i, rec.Code)
			}
		}
		if n := e.count(`SELECT COUNT(*) FROM provider_events WHERE verified = 0`); n > 20 {
			t.Errorf("stored %d forged events from one address, want at most 20", n)
		}
		// Many addresses: the account's own budget caps what is kept.
		for i := 0; i < 2000; i++ {
			e.postFrom(fmt.Sprintf("198.51.%d.%d", i/250, i%250), "pa_1", []byte(fmt.Sprintf(`{"spread":%d}`, i)))
		}
		if n := e.count(`SELECT COUNT(*) FROM provider_events WHERE verified = 0`); n > 100 {
			t.Errorf("stored %d forged events for one account, want at most 100", n)
		}
		// Genuine events still go through, even from the noisy address.
		*good = true
		for _, ip := range []string{"203.0.113.9", "192.0.2.77"} {
			if rec := e.postFrom(ip, "pa_1", []byte(`{"genuine":"`+ip+`"}`)); rec.Code != 200 {
				t.Errorf("genuine event from %s: %d", ip, rec.Code)
			}
		}
		if e.count(`SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.process'`) != 2 {
			t.Error("genuine events were not queued")
		}
	})
}

// Whatever a request is, one address cannot make more than a set number of
// them a minute.
func TestRequestCapPerAddress(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newLimited(t, db, func(o *inbound.Options) { o.RequestsPerIP = 50 })
		good := badAndGood(e, "pa_1")
		*good = true
		limited := 0
		for i := 0; i < 80; i++ {
			if rec := e.postFrom("203.0.113.9", "pa_1", []byte(fmt.Sprintf(`{"n":%d}`, i))); rec.Code == http.StatusTooManyRequests {
				limited++
			}
		}
		if limited != 30 {
			t.Errorf("%d requests were limited, want 30 of 80 at a cap of 50", limited)
		}
		if rec := e.postFrom("203.0.113.10", "pa_1", []byte(`{"n":"other"}`)); rec.Code != 200 {
			t.Errorf("another address: %d", rec.Code)
		}
	})
}
