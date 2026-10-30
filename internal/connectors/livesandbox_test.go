package connectors_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/store"
)

type alertLog struct {
	mu sync.Mutex
	al []notify.Alert
}

func (a *alertLog) Raise(_ context.Context, al notify.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.al = append(a.al, al)
	return nil
}

func (a *alertLog) all() []notify.Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]notify.Alert(nil), a.al...)
}

// insertRaw stores an account the way a hand-edited database or an old
// version would have: no create-time validation.
func (e *env) insertRaw(id, mode, cfg string) {
	e.t.Helper()
	sealed, err := e.box.Seal([]byte(cfg), secrets.AADProviderConfig(id))
	if err != nil {
		e.t.Fatal(err)
	}
	err = e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: id, OrgID: store.DefaultOrgID, Mode: mode, Connector: e.fake.Name,
			Name: id, ConfigEnc: sealed, Status: "active", AcceptedCurrencies: []string{"INR"}, CreatedAt: e.clk.t})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

const sandboxOnFake = `{"api_key":"fake_key_123","sandbox":true}`

func TestALiveAccountWithSandboxConfigIsNotLoadedAndIsReportedOnce(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		al := &alertLog{}
		e.svc.SetAlerter(al)
		e.insertRaw("pa_live_sbx", "live", sandboxOnFake)
		e.insertRaw("pa_test_sbx", "test", sandboxOnFake)
		for i := 0; i < 3; i++ {
			_, err := e.svc.Get(ctx, "pa_live_sbx")
			if !errors.Is(err, connectors.ErrSandboxOnLive) || !strings.Contains(err.Error(), "pa_live_sbx") {
				t.Fatalf("Get %d: %v", i, err)
			}
		}
		got := al.all()
		if len(got) != 1 || got[0].Kind != "provider.sandbox_on_live" || got[0].Refs["provider_account"] != "pa_live_sbx" {
			t.Errorf("alerts = %+v", got)
		}
		// A test account with the sandbox switch is fine.
		if _, err := e.svc.Get(ctx, "pa_test_sbx"); err != nil {
			t.Errorf("test account: %v", err)
		}
	})
}

func TestStartupDisablesLiveAccountsWithSandboxConfig(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		al := &alertLog{}
		e.svc.SetAlerter(al)
		e.insertRaw("pa_live_sbx", "live", sandboxOnFake)
		e.insertRaw("pa_live_ok", "live", `{"api_key":"fake_key_123"}`)
		e.insertRaw("pa_test_sbx", "test", sandboxOnFake)
		n, err := e.svc.DisableSandboxOnLive(ctx)
		if err != nil || n != 1 {
			t.Fatalf("disabled %d, %v", n, err)
		}
		status := func(id string) string {
			var pa store.ProviderAccount
			_ = e.db.Read(ctx, func(q store.Querier) (err error) { pa, err = store.GetProviderAccount(ctx, q, id); return })
			return pa.Status
		}
		if status("pa_live_sbx") != "disabled" || status("pa_live_ok") != "active" || status("pa_test_sbx") != "active" {
			t.Errorf("statuses: %s %s %s", status("pa_live_sbx"), status("pa_live_ok"), status("pa_test_sbx"))
		}
		if c := e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'provider_account.disable_sandbox_on_live' AND target = 'pa_live_sbx'"); c != 1 {
			t.Errorf("audit rows = %d", c)
		}
		if len(al.all()) != 1 {
			t.Errorf("alerts = %+v", al.all())
		}
		// Running it again finds nothing new; the account stays unusable even if re-enabled.
		if n, _ := e.svc.DisableSandboxOnLive(ctx); n != 0 {
			t.Errorf("second run disabled %d", n)
		}
	})
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}
