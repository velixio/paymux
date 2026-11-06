package fxmarket

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

func frankfurter(t *testing.T, status int, body string) (*Frankfurter, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/latest" || r.URL.Query().Get("base") != "EUR" {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Frankfurter{BaseURL: srv.URL, Client: srv.Client()}, &calls
}

func providers(p Provider) map[string]Provider {
	return map[string]Provider{ProviderFrankfurter: p}
}

func setProvider(t *testing.T, db *store.DB, name string) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		return store.SetOrgSetting(ctx, tx, store.DefaultOrgID, "fx_provider", `"`+name+`"`, time.Unix(1_800_000_000, 0))
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFrankfurterReturnsINRPerUnit(t *testing.T) {
	p, _ := frankfurter(t, 200, `{"amount":1,"base":"EUR","date":"2026-11-05","rates":{"INR":106.0395,"USD":1.1224,"JPY":177.85,"QQQ":2}}`)
	q, err := p.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q.Date != "2026-11-05" || q.Source != "frankfurter" {
		t.Errorf("quote %+v", q)
	}
	want := map[string]string{"EUR": "106.03950000", "USD": "94.47567712", "JPY": "0.59622997"}
	if len(q.Rates) != len(want) {
		t.Errorf("rates %v, want %v (no INR, no unknown currency)", q.Rates, want)
	}
	for c, r := range want {
		if q.Rates[c] != r {
			t.Errorf("%s = %s want %s", c, q.Rates[c], r)
		}
	}
}

// A rate that would not fit the conversion code's limits, or rounds to zero,
// must never be stored: it would fail the Overview page later.
func TestFrankfurterDropsUnusableRates(t *testing.T) {
	p, _ := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":0.000000000000001,"GBP":10000000000000,"JPY":-5,"CHF":0,"SEK":2}}`)
	q, err := p.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Rates) != 2 || q.Rates["SEK"] != "50.00000000" || q.Rates["EUR"] != "100.00000000" {
		t.Errorf("rates %v", q.Rates)
	}
}

func TestFrankfurterRejectsBadResponses(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"server error": {500, `oops`},
		"not json":     {200, `<html>`},
		"wrong base":   {200, `{"base":"USD","date":"2026-11-05","rates":{"INR":90,"EUR":1}}`},
		"no INR":       {200, `{"base":"EUR","date":"2026-11-05","rates":{"USD":1}}`},
		"zero INR":     {200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":0,"USD":1}}`},
		"bad date":     {200, `{"base":"EUR","date":"yesterday","rates":{"INR":100,"USD":1}}`},
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := frankfurter(t, c.status, c.body)
			if q, err := p.Fetch(ctx); err == nil {
				t.Errorf("no error, quote %+v", q)
			}
		})
	}
}

// The heart of "display only": refreshing must leave the rates that invoices
// and GST read untouched.
func TestRefreshNeverTouchesInvoiceRates(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		p, _ := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":2}}`)
		s := New(Options{DB: db, Providers: providers(p), Clock: func() time.Time { return time.Unix(1_800_000_000, 0) }})
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		err := db.Read(ctx, func(q store.Querier) error {
			got, err := store.ListMarketRatesFor(ctx, q, "USD")
			if err != nil {
				return err
			}
			if len(got) != 1 || got[0].Rate != "50.00000000" || got[0].Date != "2026-11-05" || got[0].Source != "frankfurter" {
				t.Errorf("market rates %+v", got)
			}
			stored, err := store.ListFXRates(ctx, q, store.DefaultOrgID, 10)
			if err != nil {
				return err
			}
			if len(stored) != 0 {
				t.Errorf("invoice rates changed: %+v", stored)
			}
			if _, err := store.GetFXRate(ctx, q, store.DefaultOrgID, "USD", "2026-11-05"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetFXRate: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestRefreshCallsTheProviderOnlyWhenStale(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		p, calls := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":2}}`)
		now := time.Unix(1_800_000_000, 0)
		s := New(Options{DB: db, Providers: providers(p), Clock: func() time.Time { return now }})
		for _, step := range []struct {
			after time.Duration
			want  int
		}{{0, 1}, {time.Hour, 1}, {5*time.Hour - time.Minute, 1}, {2 * time.Minute, 2}} {
			now = now.Add(step.after)
			if err := s.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			if *calls != step.want {
				t.Errorf("after +%s: %d calls, want %d", step.after, *calls, step.want)
			}
		}
	})
}

// A failed fetch keeps what was stored and reports the error, so the job retries.
func TestRefreshFailureKeepsStoredRates(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		good, _ := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":2}}`)
		now := time.Unix(1_800_000_000, 0)
		s := New(Options{DB: db, Providers: providers(good), Clock: func() time.Time { return now }})
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		bad, _ := frankfurter(t, 503, `down`)
		now = now.Add(24 * time.Hour)
		s = New(Options{DB: db, Providers: providers(bad), Clock: func() time.Time { return now }})
		if err := s.Refresh(ctx); err == nil {
			t.Error("want an error")
		}
		err := db.Read(ctx, func(q store.Querier) error {
			got, err := store.LatestMarketRates(ctx, q)
			if len(got) != 2 {
				t.Errorf("latest %+v", got)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestRefreshOffNeverCallsTheProvider(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		setProvider(t, db, "off")
		p, calls := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":2}}`)
		s := New(Options{DB: db, Providers: providers(p)})
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if *calls != 0 {
			t.Errorf("%d calls while off", *calls)
		}
	})
}

// Turning the setting back on takes effect at the next run, with no restart.
func TestRefreshFollowsTheSettingWithoutRestart(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		setProvider(t, db, "off")
		p, calls := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":2}}`)
		s := New(Options{DB: db, Providers: providers(p)})
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		setProvider(t, db, ProviderFrankfurter)
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		if *calls != 1 {
			t.Errorf("%d calls, want 1", *calls)
		}
	})
}

// A host with no outbound access must not fill the logs: a failed fetch is one
// warning, and the job succeeds so the queue does not retry it; the next hourly
// run tries again.
func TestJobTreatsAFailedFetchAsOneWarning(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		bad, calls := frankfurter(t, 503, `down`)
		var logs bytes.Buffer
		s := New(Options{DB: db, Providers: providers(bad), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
		if err := s.runJob(ctx, nil); err != nil {
			t.Errorf("job failed, the queue would retry it: %v", err)
		}
		if *calls != 1 {
			t.Errorf("%d calls", *calls)
		}
		if n := strings.Count(logs.String(), "level=WARN"); n != 1 || strings.Contains(logs.String(), "level=ERROR") {
			t.Errorf("logs: %s", logs.String())
		}
	})
}

// A database failure is ours, not the provider's: the queue must see it.
func TestJobReportsADatabaseFailure(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		p, _ := frankfurter(t, 200, `{"base":"EUR","date":"2026-11-05","rates":{"INR":100,"USD":2}}`)
		s := New(Options{DB: db, Providers: providers(p)})
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.runJob(cancelled, nil); err == nil {
			t.Error("want an error")
		}
	})
}
