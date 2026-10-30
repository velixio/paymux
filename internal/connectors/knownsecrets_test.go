package connectors_test

import (
	"bytes"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// At start-up the secrets of every stored account are handed to the redactor,
// before any of them has been used.
func TestLoadKnownSecretsCoversEveryAccount(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		a := e.create(fakeconn.ValidConfig)
		b := e.create(`{"api_key":"second_key_456"}`)
		var mu sync.Mutex
		known := map[string][]string{}
		box, _ := secrets.NewBox(bytes.Repeat([]byte{3}, 32))
		base, _ := url.Parse("https://pay.example.com")
		fresh := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: func() time.Time { return e.clk.t },
			KnownSecrets: func(owner string, v []string) { mu.Lock(); known[owner] = v; mu.Unlock() }})
		if err := fresh.LoadKnownSecrets(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(known["account:"+a.ID]) != 1 || known["account:"+a.ID][0] != "fake_key_123" ||
			len(known["account:"+b.ID]) != 1 || known["account:"+b.ID][0] != "second_key_456" {
			t.Errorf("%v", known)
		}
	})
}
