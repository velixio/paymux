package connectors_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

var guardSeq atomic.Int64

// A connector that declares DenyPrivateNetwork gets a guarded client from the
// host, unless the account config sets allow_insecure.
func TestHostGuardsConnectorsThatCallOperatorURLs(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		name := fmt.Sprintf("guarded%d", guardSeq.Add(1))
		var deps []connector.Deps
		connector.Register(name, func(_ json.RawMessage, d connector.Deps) (connector.Connector, error) {
			deps = append(deps, d)
			return nil, nil
		}, connector.Info{Version: "t", DisplayName: "Guarded", DenyPrivateNetwork: true, Currencies: []string{"INR"},
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"allow_insecure":{"type":"boolean"}}}`)})

		var hits atomic.Int32
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		defer local.Close()

		for _, tc := range []struct {
			cfg     string
			guarded bool
		}{{`{}`, true}, {`{"allow_insecure":true}`, false}} {
			pa, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
				Connector: name, Name: tc.cfg, Mode: "test", Config: json.RawMessage(tc.cfg), AppID: "app_1"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.Get(ctx, pa.ID); err != nil {
				t.Fatal(err)
			}
			resp, err := deps[len(deps)-1].HTTP.Get(local.URL)
			if tc.guarded {
				if !errors.Is(err, connector.ErrPrivateAddress) {
					t.Errorf("%s: loopback reachable through the host's client: %v", tc.cfg, err)
				}
			} else if err != nil {
				t.Errorf("%s: %v", tc.cfg, err)
			} else {
				resp.Body.Close()
			}
		}
		if hits.Load() != 1 {
			t.Errorf("local server hits = %d, want 1 (only the opted-out account)", hits.Load())
		}
	})
}
