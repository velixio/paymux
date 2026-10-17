package connectors_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/connectors/simulator"
	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/store"
)

func TestATestOnlyConnectorCannotBeLive(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		in := connectors.CreateAccountInput{Connector: simulator.Name, Name: "sim", Mode: "live", Config: json.RawMessage(`{}`), AppID: "app_1"}
		_, err := e.svc.CreateAccount(ctx, audit.System, org, in)
		if err == nil || !strings.Contains(err.Error(), "test mode") {
			t.Fatalf("live simulator account: %v", err)
		}
		var n int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM provider_accounts").Scan(&n)
		})
		if n != 0 {
			t.Errorf("a refused account was stored (%d rows)", n)
		}
		in.Mode = "test"
		pa, err := e.svc.CreateAccount(ctx, audit.System, org, in)
		if err != nil {
			t.Fatalf("test-mode simulator account: %v", err)
		}
		if _, err := e.svc.Get(ctx, pa.ID); err != nil {
			t.Fatalf("building the simulator: %v", err)
		}

		// An account that is live by some other route (a hand-edited database)
		// still cannot run.
		now := time.Now()
		err = e.db.Write(ctx, func(tx store.Tx) error {
			bad := pa
			bad.ID, bad.Mode, bad.CreatedAt = "pa_live_sim", "live", now
			return store.InsertProviderAccount(ctx, tx, bad)
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Get(ctx, "pa_live_sim"); err == nil || !strings.Contains(err.Error(), "test mode") {
			t.Errorf("a live-mode simulator account was built: %v", err)
		}
	})
}
