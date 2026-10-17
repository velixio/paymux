package connectors_test

import (
	"encoding/json"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/store"
)

// A connector that is merchant of record for some accounts only (a remote one,
// discovered per account) is recorded as such when the account is created, so
// invoicing, which reads the stored account, can see it.
func TestAccountRecordsWhetherTheLiveConnectorIsMerchantOfRecord(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		create := func(name string) store.ProviderAccount {
			pa, err := e.svc.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: name, Mode: "live",
				Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
			if err != nil {
				t.Fatal(err)
			}
			var got store.ProviderAccount
			if err := e.db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetProviderAccount(ctx, q, pa.ID); return }); err != nil {
				t.Fatal(err)
			}
			return got
		}
		if pa := create("ordinary"); pa.MerchantOfRecord {
			t.Error("an ordinary account is merchant of record")
		}
		e.fake.Locked(func() { e.fake.LiveMerchantOfRecord = true })
		if pa := create("mor"); !pa.MerchantOfRecord {
			t.Error("an account of a connector that reports itself merchant of record is not")
		}
	})
}
