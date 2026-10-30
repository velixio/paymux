package connectors_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

var epSeq atomic.Int64

// registerEndpointConnector registers a connector like the remote one: it calls
// an operator-supplied address (base_url, an endpoint field), sends it a secret,
// and may be told allow_insecure. It records the deps the host built for it.
func registerEndpointConnector(t *testing.T) (name string, deps *[]connector.Deps) {
	t.Helper()
	name = fmt.Sprintf("endpoint%d", epSeq.Add(1))
	var got []connector.Deps
	connector.Register(name, func(_ json.RawMessage, d connector.Deps) (connector.Connector, error) {
		got = append(got, d)
		return nil, nil
	}, connector.Info{Version: "t", DisplayName: "Endpoint", DenyPrivateNetwork: true, Currencies: []string{"INR"},
		ConfigSchema: json.RawMessage(`{"type":"object","required":["base_url","secret"],"properties":{
			"base_url":{"type":"string","x-endpoint":true},
			"secret":{"type":"string","x-secret":true},
			"label":{"type":"string"},
			"allow_insecure":{"type":"boolean"}}}`)})
	return name, &got
}

// allow_insecure lifts the private-address guard of a connector that calls an
// operator-supplied URL. That is for local development: a live account must not be
// able to turn the guard off, so the service refuses it with a clear error.
func TestAllowInsecureIsRefusedForLiveAccounts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		name, _ := registerEndpointConnector(t)
		cfg := `{"base_url":"http://127.0.0.1:9","secret":"s","allow_insecure":true}`
		_, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
			Connector: name, Name: "live", Mode: "live", Config: json.RawMessage(cfg), AppID: "app_1"})
		if err == nil || !strings.Contains(err.Error(), "allow_insecure") || !strings.Contains(err.Error(), "live") {
			t.Fatalf("a live account with allow_insecure was created, or the error is unclear: %v", err)
		}
		// Test mode keeps it (local development).
		if _, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
			Connector: name, Name: "test", Mode: "test", Config: json.RawMessage(cfg), AppID: "app_1"}); err != nil {
			t.Fatalf("test mode: %v", err)
		}
		// A live account cannot gain it by an update either.
		live, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
			Connector: name, Name: "live2", Mode: "live", Config: json.RawMessage(`{"base_url":"https://svc.example.com","secret":"s"}`), AppID: "app_1"})
		if err != nil {
			t.Fatal(err)
		}
		err = e.svc.UpdateAccount(ctx, audit.System, org, live.ID, connectors.UpdateAccountInput{
			Config: json.RawMessage(`{"base_url":"https://svc.example.com","secret":"s","allow_insecure":true}`)})
		if err == nil || !strings.Contains(err.Error(), "allow_insecure") {
			t.Errorf("update of a live account to allow_insecure: %v", err)
		}
	})
}

// An account that was created live with allow_insecure before the rule existed
// stays guarded: the opt-out is ignored for live accounts.
func TestLiveAccountsIgnoreAnAllowInsecureAlreadyStored(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		name, deps := registerEndpointConnector(t)
		pa, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
			Connector: name, Name: "old", Mode: "test", AppID: "app_1",
			Config: json.RawMessage(`{"base_url":"http://127.0.0.1:9","secret":"s","allow_insecure":true}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE provider_accounts SET mode = 'live' WHERE id = ?", pa.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		e.svc.Invalidate(pa.ID)
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatal(err)
		}
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer local.Close()
		if _, err := (*deps)[len(*deps)-1].HTTP.Get(local.URL); !errors.Is(err, connector.ErrPrivateAddress) {
			t.Errorf("a live account reached a loopback address through allow_insecure: %v", err)
		}
	})
}

// Editing a non-secret endpoint field must not carry the stored secret to a new
// host: the dashboard keeps the stored secret when the field is left blank, so the
// service sees a config with a new base_url and the old secret. That is refused
// until the secret is entered again.
func TestChangingAnEndpointNeedsTheSecretAgain(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		name, _ := registerEndpointConnector(t)
		pa, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
			Connector: name, Name: "svc", Mode: "test", AppID: "app_1",
			Config: json.RawMessage(`{"base_url":"https://a.example.com","secret":"s1","label":"one"}`)})
		if err != nil {
			t.Fatal(err)
		}
		update := func(cfg string) error {
			return e.svc.UpdateAccount(ctx, audit.System, org, pa.ID, connectors.UpdateAccountInput{Config: json.RawMessage(cfg)})
		}
		err = update(`{"base_url":"https://evil.example.com","secret":"s1","label":"one"}`)
		if err == nil || !strings.Contains(err.Error(), "base_url") || !strings.Contains(strings.ToLower(err.Error()), "secret") {
			t.Fatalf("a new endpoint kept the stored secret: %v", err)
		}
		if err := update(`{"base_url":"https://a.example.com","secret":"s1","label":"two"}`); err != nil {
			t.Errorf("a change of a non-endpoint field: %v", err)
		}
		if err := update(`{"base_url":"https://a.example.com/","secret":"s1","label":"two"}`); err == nil {
			t.Error("a different URL string kept the secret")
		}
		if err := update(`{"base_url":"https://b.example.com","secret":"s2","label":"two"}`); err != nil {
			t.Errorf("a new endpoint with a new secret: %v", err)
		}
		if err := update(`{"base_url":"https://b.example.com","secret":"s2","label":"three"}`); err != nil {
			t.Errorf("an unchanged endpoint: %v", err)
		}
	})
}
