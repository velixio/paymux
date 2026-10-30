package httpapi_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/logctx"
	"github.com/velixio/paymux/internal/store"
)

// An API request tells the request scope which org, app and object it is about,
// so a failure log row can be traced: org and app from the key, the object from
// the path (only when it is shaped like one of our ids).
func TestRequestsNoteTheirIdentifiers(t *testing.T) {
	var mu sync.Mutex
	var seen []map[string]string
	fixtureHook = func(ctx context.Context, method, route string, status int, d time.Duration) {
		mu.Lock()
		seen = append(seen, logctx.IDs(ctx))
		mu.Unlock()
	}
	t.Cleanup(func() { fixtureHook = nil })
	each(t, func(t *testing.T, f *fixture) {
		mu.Lock()
		seen = nil
		mu.Unlock()
		pay, rfd, sub := ids.New("pay"), ids.New("rfd"), ids.New("sub")
		f.do("GET", "/v1/payments/"+pay, f.testKey, nil)
		f.do("GET", "/v1/refunds/"+rfd, f.testKey, nil)
		f.do("GET", "/v1/subscriptions/"+sub, f.testKey, nil)
		f.do("GET", "/v1/payments/not-one-of-ours@example.com", f.testKey, nil)
		f.do("GET", "/v1/payments/"+pay, "pmx_test_sk_wrongwrongwrongwrongwrongwrongwrongwrong1", nil)
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 5 {
			t.Fatalf("%d responses seen", len(seen))
		}
		if g := seen[0]; g["org_id"] != store.DefaultOrgID || g["app_id"] != f.app.ID || g["payment_id"] != pay || g["request_id"] == "" {
			t.Errorf("payment: %v", g)
		}
		if seen[1]["refund_id"] != rfd || seen[2]["subscription_id"] != sub {
			t.Errorf("refund/subscription: %v %v", seen[1], seen[2])
		}
		if _, ok := seen[3]["payment_id"]; ok {
			t.Errorf("a path that is not one of our ids is not noted: %v", seen[3])
		}
		if _, ok := seen[4]["org_id"]; ok {
			t.Errorf("an unauthenticated request has no org: %v", seen[4])
		}
	})
}
