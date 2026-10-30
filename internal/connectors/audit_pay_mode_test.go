package connectors_test

import (
	"encoding/json"
	"testing"

	_ "github.com/velixio/paymux/connectors/payu"
	_ "github.com/velixio/paymux/connectors/stripe"
	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// AUDIT: the Paymux account mode is not bound to the provider credentials'
// mode. A LIVE-mode account can be created with PayU's sandbox switch on, or
// with a Stripe test key, so a live Paymux payment is "succeeded" by a sandbox
// payment made with a public test card (no money moves). The simulator is
// refused in live mode; real connectors are not checked at all.
func TestAuditPayLiveAccountRefusesSandboxCredentials(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		for name, tc := range map[string]struct{ conn, cfg string }{
			"payu test_mode":  {"payu", `{"key":"k","salt":"s","test_mode":true}`},
			"stripe sk_test_": {"stripe", `{"secret_key":"sk_test_0123456789","webhook_secret":"whsec_x"}`},
		} {
			_, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{
				Connector: tc.conn, Name: name, Mode: "live", Config: json.RawMessage(tc.cfg), AppID: "app_1"})
			if err == nil {
				t.Errorf("%s: a live-mode account was created with sandbox credentials", name)
			}
		}
	})
}
