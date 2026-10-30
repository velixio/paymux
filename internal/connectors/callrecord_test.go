package connectors_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

type recorded struct {
	pa  store.ProviderAccount
	rec connector.CallRecord
}

// The default per-account HTTP client reports failed provider calls with the
// account they belong to, and tells the redactor about the account's secrets.
func TestAccountClientsReportFailedCallsAndSecrets(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		box, _ := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
		var mu sync.Mutex
		var calls []recorded
		known := map[string][]string{}
		base, _ := url.Parse("https://pay.example.com")
		clk := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
		svc := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now,
			CallRecorder: func(pa store.ProviderAccount, info connector.Info) connector.CallRecorder {
				return func(_ context.Context, c connector.CallRecord) {
					mu.Lock()
					calls = append(calls, recorded{pa, c})
					mu.Unlock()
				}
			},
			KnownSecrets: func(owner string, values []string) { mu.Lock(); known[owner] = values; mu.Unlock() },
		})
		e := &env{t: t, db: db, fake: fakeconn.Register("fake"), clk: clk, box: box, svc: svc}
		if err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SeedOrg(ctx, tx, "Paymux", clk.t); err != nil {
				return err
			}
			return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: clk.t})
		}); err != nil {
			t.Fatal(err)
		}
		pa := e.create(fakeconn.ValidConfig)
		live, err := svc.Get(ctx, pa.ID)
		if err != nil || live.Connector == nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := known["account:"+pa.ID]
		mu.Unlock()
		if len(got) != 1 || got[0] != "fake_key_123" {
			t.Errorf("known secrets of the account = %v", got)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/fail" {
				w.WriteHeader(502)
				_, _ = w.Write([]byte(`{"error":"gateway"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()
		var deps connector.Deps
		e.fake.Locked(func() { deps = e.fake.Deps[len(e.fake.Deps)-1] })
		for _, p := range []string{"/ok", "/fail"} {
			resp, err := deps.HTTP.Get(srv.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		mu.Lock()
		defer mu.Unlock()
		if len(calls) != 1 || calls[0].pa.ID != pa.ID || calls[0].rec.Status != 502 || calls[0].rec.URL.Path != "/fail" {
			t.Errorf("calls = %+v", calls)
		}
	})
}
